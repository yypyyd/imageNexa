package oreate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

// Oreate refuses a generation that was not requested by a real signed page, so
// the browser both mints the token and posts the request. Opening a page for
// every generation cost 14 to 16 seconds of navigation, which is why pages are
// kept warm here: a submit is then just the two fetches the site makes. A page
// belongs to one account and pins one proxy session for its whole life, because
// the token is only valid for that account from that exit IP.
const (
	proxySessionEnv    = "OREATE_PROXY_SESSION"
	proxySessionMarker = "-session-"

	signerPagesEnv = "OREATE_SIGNER_PAGES"
	// A warm page costs about half a gigabyte of headroom and bursts one core
	// while it submits, and one page submits every one to two seconds, so a
	// handful of them already start far more generations than a host can render.
	signerPageMemoryCost = 512 << 20
	minSignerPages       = 2
	maxSignerPages       = 32

	signerMintTimeout = bantiResponseTimeout + 15*time.Second
	// A page is recycled once it goes unused, gets old or has minted enough
	// tokens, which keeps a stale document or a leaking renderer from being
	// reused forever.
	signerPageIdleTTL  = 5 * time.Minute
	signerPageMaxAge   = 30 * time.Minute
	signerPageMaxMints = 200
	signerSweepEvery   = time.Minute
)

// hotSignature is a token plus the proxy session that must carry the requests
// using it, because the token is only valid from that exit IP.
type hotSignature struct {
	Signature
	Proxy string
}

// hotSigner is implemented by signers that keep warm pages, so a test signer
// installed with SetSigner still works through the plain Signer interface.
type hotSigner interface {
	SignHot(ctx context.Context, account Account) (hotSignature, error)
}

// signerPage is one warm signed chat page: the browser stays open between
// generations so a mint costs a Banti report instead of a page load. A page
// carries one account's cookies, so it only ever signs for that account.
type signerPage struct {
	mintMu sync.Mutex
	ctx    context.Context
	close  func()
	key    string
	proxy  string
	opened time.Time
	idleAt time.Time
	mints  int
}

type signerPool struct {
	proxy func() string
	limit int

	mu   sync.Mutex
	live int
	// idle holds warm pages per account, never shared across accounts: a token
	// minted on one account's page is refused as spam for any other account.
	idle map[string][]*signerPage

	free  chan struct{}
	sweep sync.Once
}

func newSignerPool(proxy func() string) *signerPool {
	return &signerPool{
		proxy: proxy,
		limit: signerPages(),
		idle:  make(map[string][]*signerPage),
		free:  make(chan struct{}, 1),
	}
}

// signerKey identifies the account a warm page belongs to.
func signerKey(account Account) string {
	if account.OUID != "" {
		return account.OUID
	}
	if account.Email != "" {
		return account.Email
	}
	return account.Cookie
}

// signerPages sizes the warm pool from the resources this process can use, so
// the same build fits a small container and a large host; the env var stays as
// an override for hosts where the derived number is wrong.
func signerPages() int {
	if raw := strings.TrimSpace(os.Getenv(signerPagesEnv)); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			return parsed
		}
	}
	limit := availableCPUs() * 2
	if memory := availableMemory(); memory > 0 {
		limit = min(limit, int(memory/signerPageMemoryCost))
	}
	return min(max(limit, minSignerPages), maxSignerPages)
}

// submit runs one whole in-page submit on a warm page of the account and reports
// the proxy session that page egresses through, so the chat polling that follows
// is seen from the same exit IP. A page that fails is dropped and the submit
// retried once, for the same reason a mint is.
func (p *signerPool) submit(ctx context.Context, account Account, quotedPayload string) (videoSubmitResult, error) {
	result, err := p.submitOnce(ctx, account, quotedPayload)
	if err == nil || ctx.Err() != nil || errors.Is(err, ErrAuth) {
		return result, err
	}
	return p.submitOnce(ctx, account, quotedPayload)
}

func (p *signerPool) submitOnce(ctx context.Context, account Account, quotedPayload string) (videoSubmitResult, error) {
	page, err := p.acquire(ctx, account)
	if err != nil {
		return videoSubmitResult{}, err
	}
	result, err := page.submit(ctx, quotedPayload)
	p.release(page, err)
	if err != nil {
		return videoSubmitResult{}, err
	}
	result.Proxy = page.proxy
	return result, nil
}

// sign mints one token on a warm page. A page that fails is dropped and the
// mint retried once, because a dead renderer or an aborted document is far more
// likely than the account being refused.
func (p *signerPool) sign(ctx context.Context, account Account) (hotSignature, error) {
	sig, err := p.signOnce(ctx, account)
	if err == nil || ctx.Err() != nil || errors.Is(err, ErrAuth) {
		return sig, err
	}
	return p.signOnce(ctx, account)
}

func (p *signerPool) signOnce(ctx context.Context, account Account) (hotSignature, error) {
	page, err := p.acquire(ctx, account)
	if err != nil {
		return hotSignature{}, err
	}
	signature, err := page.mint(ctx)
	p.release(page, err)
	if err != nil {
		return hotSignature{}, err
	}
	return hotSignature{Signature: signature, Proxy: page.proxy}, nil
}

// acquire hands out a warm page of this account, opens one while the pool is
// below its limit, and otherwise waits for a page to come back.
func (p *signerPool) acquire(ctx context.Context, account Account) (*signerPage, error) {
	p.sweep.Do(func() { go p.sweepIdle() })
	key := signerKey(account)
	for {
		page, slot := p.checkout(key)
		if page != nil {
			return page, nil
		}
		if slot {
			page, err := p.open(account)
			if err != nil {
				p.mu.Lock()
				p.live--
				p.mu.Unlock()
				p.wake()
				return nil, err
			}
			page.key = key
			return page, nil
		}
		select {
		case <-p.free:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// checkout takes a warm page of this account, or reports that the caller may
// open one. At the limit it closes the page that has been idle longest on
// another account to free the slot, since that page cannot serve this one.
func (p *signerPool) checkout(key string) (*signerPage, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pages := p.idle[key]
	for len(pages) > 0 {
		page := pages[len(pages)-1]
		pages = pages[:len(pages)-1]
		if page.usable() {
			p.store(key, pages)
			return page, false
		}
		p.retire(page)
	}
	p.store(key, pages)
	if p.live >= p.limit {
		victim, victimKey := p.oldestIdle()
		if victim == nil {
			return nil, false
		}
		p.drop(victimKey, victim)
		p.retire(victim)
	}
	p.live++
	return nil, true
}

func (p *signerPool) store(key string, pages []*signerPage) {
	if len(pages) == 0 {
		delete(p.idle, key)
		return
	}
	p.idle[key] = pages
}

// oldestIdle picks the least recently used idle page across all accounts.
func (p *signerPool) oldestIdle() (*signerPage, string) {
	var oldest *signerPage
	oldestKey := ""
	for key, pages := range p.idle {
		for _, page := range pages {
			if oldest == nil || page.idleAt.Before(oldest.idleAt) {
				oldest, oldestKey = page, key
			}
		}
	}
	return oldest, oldestKey
}

func (p *signerPool) drop(key string, page *signerPage) {
	pages := p.idle[key]
	for i, candidate := range pages {
		if candidate == page {
			p.store(key, append(pages[:i], pages[i+1:]...))
			return
		}
	}
}

// retire closes a page and gives its slot back; the pool lock is held.
func (p *signerPool) retire(page *signerPage) {
	page.close()
	p.live--
}

func (p *signerPool) release(page *signerPage, err error) {
	if err != nil || !page.usable() {
		p.discard(page)
		return
	}
	page.idleAt = time.Now()
	p.mu.Lock()
	p.idle[page.key] = append(p.idle[page.key], page)
	p.mu.Unlock()
	p.wake()
}

func (p *signerPool) discard(page *signerPage) {
	p.mu.Lock()
	p.drop(page.key, page)
	p.retire(page)
	p.mu.Unlock()
	p.wake()
}

// wake releases one waiter without blocking when none is waiting.
func (p *signerPool) wake() {
	select {
	case p.free <- struct{}{}:
	default:
	}
}

// sweepIdle closes pages nothing has needed for a while so a quiet instance
// stops paying for browsers it is not using.
func (p *signerPool) sweepIdle() {
	ticker := time.NewTicker(signerSweepEvery)
	defer ticker.Stop()
	for range ticker.C {
		p.mu.Lock()
		for key, pages := range p.idle {
			kept := pages[:0]
			for _, page := range pages {
				if page.usable() && time.Since(page.idleAt) < signerPageIdleTTL {
					kept = append(kept, page)
					continue
				}
				p.retire(page)
			}
			p.store(key, kept)
		}
		p.mu.Unlock()
		p.wake()
	}
}

// open launches one browser pinned to a fresh proxy session, installs the
// account cookies and waits for the page runtime that mints tokens.
func (p *signerPool) open(account Account) (*signerPage, error) {
	path := chromiumExecPath()
	if path == "" {
		return nil, errors.New("oreate signer: no Chrome/Chromium binary (set OREATE_CHROME)")
	}
	proxyURL := ""
	if p.proxy != nil {
		proxyURL = stickyProxyURL(p.proxy(), newProxySession())
	}
	// The page outlives the generation that opened it, so its browser hangs off
	// the background context and is only closed by the pool.
	pageCtx, cancelPage := context.WithCancel(context.Background())
	opts, bridge, err := browserExecOptions(pageCtx, path, account, proxyURL)
	if err != nil {
		cancelPage()
		return nil, err
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(pageCtx, opts...)
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	page := &signerPage{
		ctx:    browserCtx,
		proxy:  proxyURL,
		opened: time.Now(),
		idleAt: time.Now(),
	}
	page.close = sync.OnceFunc(func() {
		cancelBrowser()
		cancelAlloc()
		cancelPage()
		if bridge != nil {
			bridge.Close()
		}
	})
	// chromedp ties the browser process to whichever context first runs an
	// action, so the browser is started on the page's own context: starting it
	// from a bounded one would take the browser down with that bound.
	started := make(chan error, 1)
	go func() { started <- chromedp.Run(browserCtx) }()
	select {
	case err := <-started:
		if err != nil {
			page.close()
			return nil, fmt.Errorf("oreate signer: browser start: %w", err)
		}
	case <-time.After(signerTimeout):
		page.close()
		return nil, errors.New("oreate signer: browser start timeout")
	}
	if err := page.prepare(account); err != nil {
		page.close()
		return nil, err
	}
	return page, nil
}

// prepare loads the signed chat page the same way the one-shot signer does, and
// bounds itself so a hanging proxy cannot block the pool.
func (page *signerPage) prepare(account Account) error {
	ctx, cancel := context.WithTimeout(page.ctx, signerTimeout)
	defer cancel()
	if err := chromedp.Run(ctx, signerCookieActions(account)...); err != nil {
		return fmt.Errorf("oreate signer: cookie setup: %w", err)
	}
	navErr := navigateSignerPage(ctx)
	if err := chromedp.Run(ctx, chromedp.Poll(`typeof window.paris_21a851acb0 === "object"`, nil,
		chromedp.WithPollingInterval(100*time.Millisecond), chromedp.WithPollingTimeout(25*time.Second))); err != nil {
		diagnostics := browserRuntimeDiagnostics(ctx)
		if navErr != nil {
			return fmt.Errorf("oreate signer: page navigation: %w (%s)", navErr, diagnostics)
		}
		return fmt.Errorf("oreate signer: Paris runtime: %w (%s)", err, diagnostics)
	}
	return nil
}

// mint asks the page runtime for one token. Only one mint runs on a page at a
// time because the report state lives on the document.
func (page *signerPage) mint(ctx context.Context) (Signature, error) {
	page.mintMu.Lock()
	defer page.mintMu.Unlock()
	runCtx, cancel, err := page.runContext(ctx, signerMintTimeout)
	if err != nil {
		return Signature{}, err
	}
	defer cancel()
	if err := chromedp.Run(runCtx, chromedp.Evaluate(hotMintScript, nil)); err != nil {
		return Signature{}, fmt.Errorf("oreate signer: Banti dispatch: %w", err)
	}
	var jt, browserCookie string
	if err := chromedp.Run(runCtx,
		chromedp.Poll(`window.__oreateSignerJT`, &jt,
			chromedp.WithPollingInterval(25*time.Millisecond), chromedp.WithPollingTimeout(bantiResponseTimeout)),
		chromedp.Evaluate(`document.cookie`, &browserCookie),
	); err != nil {
		return Signature{}, fmt.Errorf("oreate signer: Banti response: %w", err)
	}
	page.mints++
	jt = strings.TrimSpace(jt)
	if jt == "" || len(jt) > maxBantiJTLength {
		return Signature{}, ErrRiskControl
	}
	return Signature{JT: jt, BID: cookieValue(browserCookie, "__bid_n"), Cookie: strings.TrimSpace(browserCookie)}, nil
}

// submit mints a token and posts the generation from the page itself, which is
// what risk control accepts. Only one submit runs on a page at a time because
// the request state lives on the document.
func (page *signerPage) submit(ctx context.Context, quotedPayload string) (videoSubmitResult, error) {
	page.mintMu.Lock()
	defer page.mintMu.Unlock()
	runCtx, cancel, err := page.runContext(ctx, inPageSubmitTimeout)
	if err != nil {
		return videoSubmitResult{}, err
	}
	defer cancel()
	started := time.Now()
	if err := chromedp.Run(runCtx, chromedp.Evaluate(inPageSubmitScript(quotedPayload), nil)); err != nil {
		return videoSubmitResult{}, fmt.Errorf("oreate signer: submit dispatch: %w", err)
	}
	pollErr := chromedp.Run(runCtx, chromedp.Poll(`window.__oreateSubmit && window.__oreateSubmit.done === true`, nil,
		chromedp.WithPollingInterval(250*time.Millisecond), chromedp.WithPollingTimeout(inPageAcceptWait)))
	var result videoSubmitResult
	if err := chromedp.Run(runCtx, chromedp.Evaluate(`window.__oreateSubmit || {}`, &result)); err != nil {
		if pollErr != nil {
			return videoSubmitResult{}, fmt.Errorf("oreate signer: submit stream: %w", pollErr)
		}
		return videoSubmitResult{}, fmt.Errorf("oreate signer: submit state: %w", err)
	}
	if pollErr != nil && result.Failure == "" {
		result.Failure = pollErr.Error()
	}
	page.mints++
	// A warm page reduces a submit to the two fetches the site itself makes, and
	// that cost is what decides how many generations a host can start per minute.
	log.Printf("oreate inpage submit: submits=%d accept=%s chat=%s", page.mints,
		time.Since(started).Round(time.Millisecond), result.ChatID)
	return result, nil
}

// runContext bounds one page operation by the shorter of the caller's deadline
// and the operation's own budget, while keeping the page's own lifetime.
func (page *signerPage) runContext(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc, error) {
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline); left < budget {
			budget = left
		}
	}
	if budget <= 0 {
		return nil, nil, context.DeadlineExceeded
	}
	runCtx, cancel := context.WithTimeout(page.ctx, budget)
	return runCtx, cancel, nil
}

func (page *signerPage) usable() bool {
	return page.ctx.Err() == nil && page.mints < signerPageMaxMints && time.Since(page.opened) < signerPageMaxAge
}

// hotMintScript mints one token on a page that is already loaded. The token is
// published on the window so the Go side can poll for it, and a non-empty value
// means the Banti report the site requires went through.
const hotMintScript = `window.__oreateSignerJT = "";
	window.paris_21a851acb0.getBantiInstance((instanceError, instance) => {
		if (instanceError || !instance) return;
		if (instance.options) instance.options.reportTimeout = 20000;
		window.paris_21a851acb0.sendBantiReport({subid: ""}, (reportError, response) => {
			if (reportError) return;
			window.__oreateSignerJT = response && response.htj && response.htj.jt || "";
		});
	}); true`

// stickyProxyURL pins the proxy to one exit IP by labelling the session in the
// user name, which is how rotating residential pools expose sticky sessions.
// Pools that do not support the label, or a proxy without credentials, are left
// alone; OREATE_PROXY_SESSION=false turns the rewrite off entirely.
func stickyProxyURL(raw, session string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || session == "" || strings.EqualFold(strings.TrimSpace(os.Getenv(proxySessionEnv)), "false") {
		return raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw
	}
	name := parsed.User.Username()
	if name == "" || strings.Contains(name, proxySessionMarker) {
		return raw
	}
	password, _ := parsed.User.Password()
	parsed.User = url.UserPassword(name+proxySessionMarker+session, password)
	return parsed.String()
}

func newProxySession() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf)
}
