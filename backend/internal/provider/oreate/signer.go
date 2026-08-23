package oreate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

const maxBantiJTLength = 4096

const (
	signerTimeout          = 75 * time.Second
	bantiResponseTimeout   = 25 * time.Second
	signerNavigateAttempts = 3
)

var signerBlockedURLs = []string{
	"*://scripts.clarity.ms/*",
	"*://www.clarity.ms/*",
	"*://connect.facebook.net/*",
	"*://bat.bing.com/*",
	"*://www.googletagmanager.com/*",
	"*://googleads.g.doubleclick.net/*",
	"*://r.wdfl.co/*",
	"*://us-assets.i.posthog.com/*",
	"*://accounts.google.com/*",
}

type chromiumSigner struct {
	proxy func() string
	sem   chan struct{}
	// pool keeps the warm signed pages that mint the tokens and post the
	// generations, which is what bounds how many submits run at once.
	pool  *signerPool
	probe func(signerProbeEvent)
}

type signerProbeEvent struct {
	Kind      string
	Target    string
	Status    int64
	Error     string
	ElapsedMS int64
}

type signerRuntimeProbe struct {
	StartedAt           float64 `json:"startedAt"`
	InstanceAt          float64 `json:"instanceAt"`
	SendAt              float64 `json:"sendAt"`
	CallbackAt          float64 `json:"callbackAt"`
	InstanceError       bool    `json:"instanceError"`
	InstancePresent     bool    `json:"instancePresent"`
	OptionsPresent      bool    `json:"optionsPresent"`
	ReportTimeoutBefore float64 `json:"reportTimeoutBefore"`
	ReportTimeoutAfter  float64 `json:"reportTimeoutAfter"`
	InstanceSend        bool    `json:"instanceSend"`
	GlobalSend          bool    `json:"globalSend"`
	CallbackError       bool    `json:"callbackError"`
	ResponsePresent     bool    `json:"responsePresent"`
	HTJPresent          bool    `json:"htjPresent"`
	JTLength            int     `json:"jtLength"`
}

type chromiumProxy struct {
	server       string
	username     string
	password     string
	authenticate bool
}

func newChromiumSigner(proxy func() string) *chromiumSigner {
	return &chromiumSigner{
		proxy: proxy,
		sem:   make(chan struct{}, 2),
		pool:  newSignerPool(proxy),
	}
}

// SignHot mints a token on a warm page and reports the proxy session that page
// uses, which the caller has to reuse for the requests carrying the token.
func (s *chromiumSigner) SignHot(ctx context.Context, account Account) (hotSignature, error) {
	account = account.normalized()
	if account.Cookie == "" {
		return hotSignature{}, ErrAuth
	}
	return s.pool.sign(ctx, account)
}

func (s *chromiumSigner) Sign(ctx context.Context, account Account) (Signature, error) {
	account = account.normalized()
	if account.Cookie == "" {
		return Signature{}, ErrAuth
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return Signature{}, ctx.Err()
	}

	path := chromiumExecPath()
	if path == "" {
		return Signature{}, errors.New("oreate signer: no Chrome/Chromium binary (set OREATE_CHROME)")
	}
	// Opening the page through a residential proxy times out intermittently
	// ("page navigation" / "Paris runtime" in production logs) and a second
	// browser normally gets through, so retry that class of failure once.
	sig, err := s.signOnce(ctx, path, account)
	if err != nil {
		if !isTransientSignerFailure(err) || ctx.Err() != nil {
			return Signature{}, err
		}
		sig, err = s.signOnce(ctx, path, account)
		if err != nil {
			return Signature{}, err
		}
	}
	// Keep the opaque browser token ephemeral and reject empty or implausibly
	// large values before they can reach the website API.
	if len(sig.JT) == 0 || len(sig.JT) > maxBantiJTLength {
		return Signature{}, ErrRiskControl
	}
	return sig, nil
}

// isTransientSignerFailure reports whether the browser failed to reach a usable
// page (navigation / runtime timeouts) rather than the account being rejected.
func isTransientSignerFailure(err error) bool {
	if err == nil || errors.Is(err, ErrAuth) || errors.Is(err, ErrRiskControl) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "page navigation") ||
		strings.Contains(msg, "Paris runtime") ||
		strings.Contains(msg, "cookie setup")
}

// navigateSignerPage opens the chat page, reloading it in the same browser when
// the document aborts mid-stream (proxy timeouts, ERR_INCOMPLETE_CHUNKED_ENCODING)
// because that is far cheaper than starting another browser.
func navigateSignerPage(ctx context.Context) error {
	var err error
	for attempt := 0; attempt < signerNavigateAttempts; attempt++ {
		if err = chromedp.Run(ctx, chromedp.Navigate(defaultRefer)); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return err
}

// browserOptions builds the Chrome flags for a one-shot signing browser. The
// caller closes the returned bridge once the browser exits.
func (s *chromiumSigner) browserOptions(ctx context.Context, path string, account Account) ([]chromedp.ExecAllocatorOption, *proxyBridge, error) {
	raw := ""
	if s.proxy != nil {
		raw = s.proxy()
	}
	return browserExecOptions(ctx, path, account, raw)
}

// browserExecOptions builds the flags for one browser on the given proxy URL.
func browserExecOptions(ctx context.Context, path string, account Account, proxyURL string) ([]chromedp.ExecAllocatorOption, *proxyBridge, error) {
	proxy, err := parseChromiumProxy(proxyURL)
	if err != nil {
		return nil, nil, err
	}
	browserProxy := proxy
	var bridge *proxyBridge
	if proxy.authenticate {
		bridge, err = startProxyBridge(ctx, proxy)
		if err != nil {
			return nil, nil, err
		}
		browserProxy.server = bridge.URL()
		browserProxy.authenticate = false
	}
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		chromedp.ExecPath(path),
		chromedp.Flag("headless", "new"),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-features", "Translate,BlinkGenPropertyTrees"),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.UserAgent(account.UserAgent),
	)
	if strings.EqualFold(strings.TrimSpace(os.Getenv("OREATE_NO_SANDBOX")), "true") {
		opts = append(opts, chromedp.Flag("no-sandbox", true))
	}
	opts = append(opts, browserIsolationOptions()...)
	if browserProxy.server != "" {
		opts = append(opts, chromedp.ProxyServer(browserProxy.server))
	}
	if bridge != nil {
		// Chrome otherwise bypasses loopback proxies by default, which would
		// silently skip the authenticated bridge and use direct egress.
		opts = append(opts, chromedp.Flag("proxy-bypass-list", "<-loopback>"))
	}
	return opts, bridge, nil
}

// signerCookieActions installs the account cookies on the website origin and
// keeps third-party trackers from slowing the page down.
func signerCookieActions(account Account) []chromedp.Action {
	actions := []chromedp.Action{network.Enable(), chromedp.ActionFunc(setSignerBlockedURLs)}
	for _, part := range strings.Split(account.Cookie, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || name == "" {
			continue
		}
		actions = append(actions, network.SetCookie(name, value).WithDomain(".oreateai.com").WithPath("/"))
	}
	return actions
}

func (s *chromiumSigner) signOnce(parent context.Context, path string, account Account) (Signature, error) {
	ctx, cancel := context.WithTimeout(parent, signerTimeout)
	defer cancel()
	opts, bridge, err := s.browserOptions(ctx, path, account)
	if err != nil {
		return Signature{}, err
	}
	if bridge != nil {
		defer bridge.Close()
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	defer cancelAlloc()
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()
	reportWaiter := newBantiReportWaiter()
	chromedp.ListenTarget(browserCtx, reportWaiter.listen)
	if s.probe != nil {
		listenForSignerProbe(browserCtx, s.probe)
	}

	if err := chromedp.Run(browserCtx, signerCookieActions(account)...); err != nil {
		return Signature{}, fmt.Errorf("oreate signer: cookie setup: %w", err)
	}
	navErr := navigateSignerPage(browserCtx)
	pageDiagnostics := browserRuntimeDiagnostics(browserCtx)
	if err := chromedp.Run(browserCtx, chromedp.Poll(`typeof window.paris_21a851acb0 === "object"`, nil,
		chromedp.WithPollingInterval(100*time.Millisecond), chromedp.WithPollingTimeout(25*time.Second))); err != nil {
		if navErr != nil {
			return Signature{}, fmt.Errorf("oreate signer: page navigation: %w (%s)", navErr, pageDiagnostics)
		}
		return Signature{}, fmt.Errorf("oreate signer: Paris runtime: %w (%s)", err, pageDiagnostics)
	}
	var jt, browserCookie string
	reportWaiter.arm()
	if err := chromedp.Run(browserCtx, chromedp.Evaluate(`window.__oreateSignerJT = "";
			window.__oreateSignerDiag = {startedAt: performance.now()};
			window.paris_21a851acb0.getBantiInstance((_instanceError, instance) => {
				const diag = window.__oreateSignerDiag;
				diag.instanceAt = performance.now();
				diag.instanceError = Boolean(_instanceError);
				diag.instancePresent = Boolean(instance);
				diag.optionsPresent = Boolean(instance && instance.options);
				diag.reportTimeoutBefore = Number(instance && instance.options && instance.options.reportTimeout) || 0;
				if (instance && instance.options) instance.options.reportTimeout = 20000;
				diag.reportTimeoutAfter = Number(instance && instance.options && instance.options.reportTimeout) || 0;
				diag.instanceSend = Boolean(instance && typeof instance.sendBantiReport === "function");
				diag.globalSend = typeof window.paris_21a851acb0.sendBantiReport === "function";
				diag.sendAt = performance.now();
				window.paris_21a851acb0.sendBantiReport({subid: ""}, (_error, response) => {
					const jt = response && response.htj && response.htj.jt || "";
					diag.callbackAt = performance.now();
					diag.callbackError = Boolean(_error);
					diag.responsePresent = Boolean(response);
					diag.htjPresent = Boolean(response && response.htj);
					diag.jtLength = typeof jt === "string" ? jt.length : 0;
					window.__oreateSignerJT = jt;
				});
			}); true`, nil)); err != nil {
		return Signature{}, fmt.Errorf("oreate signer: Banti dispatch: %w", err)
	}
	if err := chromedp.Run(browserCtx,
		chromedp.Poll(`window.__oreateSignerJT`, &jt,
			chromedp.WithPollingInterval(25*time.Millisecond), chromedp.WithPollingTimeout(bantiResponseTimeout)),
		chromedp.Evaluate(`document.cookie`, &browserCookie),
	); err != nil {
		s.reportRuntimeProbe(browserCtx)
		return Signature{}, fmt.Errorf("oreate signer: Banti response: %w", err)
	}
	s.reportRuntimeProbe(browserCtx)
	if err := reportWaiter.wait(browserCtx, bantiResponseTimeout); err != nil {
		return Signature{}, fmt.Errorf("oreate signer: Banti report: %w", err)
	}
	return Signature{JT: strings.TrimSpace(jt), BID: cookieValue(browserCookie, "__bid_n"), Cookie: strings.TrimSpace(browserCookie)}, nil
}

type bantiReportResult struct {
	status int64
	failed bool
}

type bantiReportWaiter struct {
	armed    atomic.Bool
	requests sync.Map
	done     chan bantiReportResult
	once     sync.Once
}

func newBantiReportWaiter() *bantiReportWaiter {
	return &bantiReportWaiter{done: make(chan bantiReportResult, 1)}
}

func (w *bantiReportWaiter) arm() {
	w.armed.Store(true)
}

func (w *bantiReportWaiter) listen(event any) {
	switch event := event.(type) {
	case *network.EventRequestWillBeSent:
		if w.armed.Load() && isBantiReportURL(event.Request.URL) {
			w.requests.Store(event.RequestID, int64(0))
		}
	case *network.EventResponseReceived:
		if _, ok := w.requests.Load(event.RequestID); ok {
			w.requests.Store(event.RequestID, event.Response.Status)
		}
	case *network.EventLoadingFinished:
		value, ok := w.requests.LoadAndDelete(event.RequestID)
		if !ok {
			return
		}
		status, _ := value.(int64)
		w.finish(bantiReportResult{status: status})
	case *network.EventLoadingFailed:
		if _, ok := w.requests.LoadAndDelete(event.RequestID); ok {
			w.finish(bantiReportResult{failed: true})
		}
	}
}

func (w *bantiReportWaiter) finish(result bantiReportResult) {
	w.once.Do(func() { w.done <- result })
}

func (w *bantiReportWaiter) wait(ctx context.Context, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-w.done:
		if result.failed || result.status < http.StatusOK || result.status >= http.StatusMultipleChoices {
			return ErrRiskControl
		}
		return nil
	case <-timer.C:
		return ErrRiskControl
	case <-ctx.Done():
		return ctx.Err()
	}
}

func isBantiReportURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && strings.EqualFold(parsed.Hostname(), "banti.oreateai.com") && parsed.EscapedPath() == "/dr"
}

func (s *chromiumSigner) reportRuntimeProbe(ctx context.Context) {
	if s.probe == nil {
		return
	}
	var diag signerRuntimeProbe
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__oreateSignerDiag || {}`, &diag)); err != nil {
		return
	}
	s.probe(signerProbeEvent{
		Kind: "diagnostic",
		Target: fmt.Sprintf(
			"instance_error=%t instance=%t options=%t "+
				"timeout_before=%.0f timeout_after=%.0f "+
				"instance_send=%t global_send=%t callback_error=%t "+
				"response=%t htj=%t jt_length=%d "+
				"instance_ms=%.0f send_ms=%.0f callback_ms=%.0f",
			diag.InstanceError, diag.InstancePresent, diag.OptionsPresent,
			diag.ReportTimeoutBefore, diag.ReportTimeoutAfter,
			diag.InstanceSend, diag.GlobalSend, diag.CallbackError,
			diag.ResponsePresent, diag.HTJPresent, diag.JTLength,
			diag.InstanceAt-diag.StartedAt, diag.SendAt-diag.StartedAt,
			diag.CallbackAt-diag.StartedAt,
		),
	})
}

func listenForSignerProbe(ctx context.Context, report func(signerProbeEvent)) {
	requests := sync.Map{}
	startedAt := time.Now()
	chromedp.ListenTarget(ctx, func(event any) {
		switch event := event.(type) {
		case *network.EventRequestWillBeSent:
			target := probeURLTarget(event.Request.URL)
			requests.Store(event.RequestID, target)
			report(signerProbeEvent{
				Kind:      "request",
				Target:    target,
				ElapsedMS: time.Since(startedAt).Milliseconds(),
			})
		case *network.EventResponseReceived:
			report(signerProbeEvent{
				Kind:      "response",
				Target:    probeURLTarget(event.Response.URL),
				Status:    event.Response.Status,
				ElapsedMS: time.Since(startedAt).Milliseconds(),
			})
		case *network.EventLoadingFailed:
			target, _ := requests.Load(event.RequestID)
			targetText, _ := target.(string)
			report(signerProbeEvent{
				Kind:      "failed",
				Target:    targetText,
				Error:     event.ErrorText,
				ElapsedMS: time.Since(startedAt).Milliseconds(),
			})
		}
	})
}

func probeURLTarget(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "unknown"
	}
	return strings.ToLower(parsed.Host) + parsed.EscapedPath()
}

func setSignerBlockedURLs(ctx context.Context) error {
	params := struct {
		URLs []string `json:"urls"`
	}{URLs: signerBlockedURLs}
	return cdp.Execute(ctx, network.CommandSetBlockedURLs, &params, nil)
}

func browserRuntimeDiagnostics(ctx context.Context) string {
	var diagnostics string
	_ = chromedp.Run(ctx, chromedp.Evaluate(`JSON.stringify({
		title: document.title || "",
		ready: document.readyState || "",
		scripts: document.scripts ? document.scripts.length : 0,
		bodyBytes: document.body && document.body.innerText ? document.body.innerText.length : 0,
		parisKeys: Object.getOwnPropertyNames(window).filter((key) => /paris/i.test(key)).slice(0, 20),
		bantiKeys: Object.getOwnPropertyNames(window).filter((key) => /banti/i.test(key)).slice(0, 20)
	})`, &diagnostics))
	if strings.TrimSpace(diagnostics) == "" {
		return "diagnostics-unavailable"
	}
	return diagnostics
}

func parseChromiumProxy(raw string) (chromiumProxy, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return chromiumProxy{}, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || parsed.Host == "" || parsed.Scheme == "" ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return chromiumProxy{}, errors.New("oreate signer: invalid proxy URL")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	switch parsed.Scheme {
	case "http", "https", "socks4", "socks5":
	default:
		return chromiumProxy{}, errors.New("oreate signer: unsupported proxy scheme")
	}

	result := chromiumProxy{}
	if parsed.User != nil {
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return chromiumProxy{}, errors.New("oreate signer: authenticated proxy must use HTTP or HTTPS")
		}
		result.username = parsed.User.Username()
		result.password, _ = parsed.User.Password()
		result.authenticate = true
	}
	parsed.User = nil
	parsed.Path = ""
	parsed.RawPath = ""
	result.server = parsed.String()
	return result, nil
}

func chromiumExecPath() string {
	if configured := strings.TrimSpace(os.Getenv("OREATE_CHROME")); configured != "" {
		if _, err := os.Stat(configured); err == nil {
			return configured
		}
	}
	for _, name := range []string{"google-chrome-stable", "google-chrome", "chromium", "chromium-browser"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	if runtime.GOOS == "windows" {
		for _, path := range []string{
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		} {
			if _, err := os.Stat(path); err == nil {
				return path
			}
		}
	}
	return ""
}
