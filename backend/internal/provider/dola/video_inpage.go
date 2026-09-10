package dola

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"backend/internal/provider/proxybridge"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

const dolaPageReadyWait = 30 * time.Second

// bootstrapBrowserSession retains the legacy browser readiness probe.
func (c *Client) bootstrapBrowserSession(ctx context.Context, account Account) error {
	lock := c.browserLock()
	for !lock.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer lock.Unlock()
	pageCtx, cleanup, err := c.openPortableBrowser(ctx, account)
	if err != nil {
		return err
	}
	defer cleanup()
	return prepareDolaControlsWithRecovery(pageCtx, DefaultVideoModel, "16:9", 30)
}

func (c *Client) openPortableBrowser(ctx context.Context, account Account) (context.Context, func(), error) {
	binding, bindingErr := loadBrowserBinding(account)
	if bindingErr != nil {
		return nil, func() {}, bindingErr
	}
	if binding != nil {
		return c.openBoundBrowser(ctx, account, *binding)
	}
	path := dolaChromiumExecPath()
	if path == "" {
		return nil, func() {}, bootstrapFailure("chromium", errors.New("chromium is not configured"))
	}
	proxyURL, err := c.AccountProxy(ctx, account)
	if err != nil {
		return nil, func() {}, bootstrapFailure("proxy", err)
	}
	pageCtx, cleanup, fresh, err := c.acquirePortableBrowser(ctx, account, path, proxyURL)
	if err != nil {
		return nil, func() {}, err
	}
	if fresh {
		err = bootstrapDolaPage(pageCtx, account)
	} else {
		err = navigateDolaPage(pageCtx, apiBase+"/chat/")
		if err == nil {
			err = waitDolaDocument(pageCtx)
		}
	}
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return pageCtx, cleanup, nil
}

func bootstrapDolaPage(ctx context.Context, account Account) error {
	// A fresh profile has no VM-local device state. Install only the allowlisted
	// account cookies with their captured domains, then let the canonical official
	// page create this server's TTWid/localStorage identity during its single load.
	var existingCookie string
	_ = chromedp.Run(ctx, network.Enable(), chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		existingCookie, err = browserDolaCookieHeader(ctx)
		return err
	}))
	if CookieValue(existingCookie, "sessionid") != CookieValue(account.Cookie, "sessionid") {
		if err := chromedp.Run(ctx, dolaCookieActions(account)...); err != nil {
			return bootstrapFailure("cookie install", err)
		}
	}
	if err := navigateDolaPage(ctx, apiBase+"/chat/"); err != nil {
		return bootstrapFailure("account navigation", err)
	}
	if err := waitDolaDocument(ctx); err != nil {
		return bootstrapFailure("account document", err)
	}
	return nil
}

func waitDolaDocument(ctx context.Context) error {
	deadline := time.Now().Add(dolaPageReadyWait)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		var ready bool
		err := chromedp.Run(ctx, chromedp.Evaluate(`location.hostname === "www.dola.com" && document.querySelector("#root") !== null && document.body !== null`, &ready))
		if err == nil && ready {
			// Require one short quiet period, then re-check the same final document.
			_ = chromedp.Run(ctx, chromedp.Sleep(750*time.Millisecond))
			ready = false
			err = chromedp.Run(ctx, chromedp.Evaluate(`location.hostname === "www.dola.com" && document.querySelector("#root") !== null && document.body !== null`, &ready))
			if err == nil && ready {
				return nil
			}
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return errors.New("dola document did not stabilize")
}

func prepareDolaVideoForm(ctx context.Context, model, prompt, ratio string, duration int) error {
	// Ensure responsive layout exposes the same action bar captured from the
	// official desktop page rather than folding Video into the More menu.
	if err := chromedp.Run(ctx, chromedp.EmulateViewport(1280, 900)); err != nil {
		return fmt.Errorf("viewport: %w", err)
	}
	if err := chromedp.Run(ctx,
		chromedp.Poll(`(() => document.querySelector("#skill-modal-video-skill, [data-testid=skill-modal-video-skill]") !== null || Array.from(document.querySelectorAll("button")).some(button => /create\s*videos?/i.test((button.textContent || "").trim()) || (button.textContent || "").includes("视频生成") || (button.textContent || "").includes("生成视频")))()`, nil, chromedp.WithPollingInterval(100*time.Millisecond), chromedp.WithPollingTimeout(dolaPageReadyWait)),
	); err != nil {
		return fmt.Errorf("video-mode wait: %w", err)
	}
	var opened bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(`(() => {
  if (document.querySelector("#skill-modal-video-skill, [data-testid=skill-modal-video-skill]")) return true;
  const visible = element => { const rect = element.getBoundingClientRect(); return rect.width > 0 && rect.height > 0; };
  const button = Array.from(document.querySelectorAll("button")).find(button => {
		const text = (button.textContent || "").trim();
		return visible(button) && (text.includes("Create Videos") || text.includes("视频生成") || text.includes("生成视频") || text.includes("Create Video"));
  });
  if (!button) return false;
  button.click();
  return true;
})()`, &opened)); err != nil || !opened {
		return errors.New("dola video mode unavailable")
	}
	if err := chromedp.Run(ctx, chromedp.Poll(`(() => Array.from(document.querySelectorAll('[contenteditable="true"], textarea')).some(element => {
  const rect = element.getBoundingClientRect(); return rect.width > 100 && rect.height > 0;
}))()`, nil, chromedp.WithPollingInterval(100*time.Millisecond), chromedp.WithPollingTimeout(dolaPageReadyWait))); err != nil {
		return fmt.Errorf("video-input wait: %w", err)
	}

	// The editor exists before the lazy video skill has mounted. Wait for its
	// model selector before filling or sending, otherwise the click only opens it.
	if err := chromedp.Run(ctx, chromedp.WaitVisible(
		"[data-input-engine-actionbar-control-key=video-model]", chromedp.ByQuery)); err != nil {
		return fmt.Errorf("video controls: %w", err)
	}
	if NormalizeModelID(model) != DefaultVideoModel {
		return errors.New("dola browser model unavailable")
	}
	if err := selectDolaVideoOption(ctx,
		`document.querySelector("[data-input-engine-actionbar-control-key=video-model]")`, "Dreamina Seedance 2.5", "model"); err != nil {
		return fmt.Errorf("model select: %w", err)
	}

	if err := chooseDolaDuration(ctx, duration); err != nil {
		return fmt.Errorf("duration select: %w", err)
	}

	if err := selectDolaVideoOption(ctx,
		`Array.from(document.querySelectorAll("button")).find(b => /^(Ratio|比例|画幅|\d+:\d+)$/.test(b.textContent.trim()))`,
		ratio, "ratio"); err != nil {
		return fmt.Errorf("ratio select: %w", err)
	}

	if strings.TrimSpace(prompt) == "" {
		return nil
	}
	var focused bool
	focusScript := `(() => {
  const visible = element => { const rect = element.getBoundingClientRect(); return rect.width > 100 && rect.height > 0; };
  const input = Array.from(document.querySelectorAll('[contenteditable="true"]')).find(visible) || Array.from(document.querySelectorAll("textarea")).find(visible);
  if (!input) return false;
  input.setAttribute("data-dola-video-input", "true");
  input.focus();
  return document.activeElement === input;
})()`
	if err := chromedp.Run(ctx, chromedp.Evaluate(focusScript, &focused)); err != nil || !focused {
		return errors.New("dola video prompt input unavailable")
	}
	// Send native keyboard events to the current focus. The editor may rerender
	// after each event, so a selector-based SendKeys can become stale mid-input.
	if err := chromedp.Run(ctx, chromedp.KeyEvent(prompt), chromedp.Sleep(500*time.Millisecond)); err != nil {
		return errors.New("dola video prompt input failed")
	}
	return nil
}

func selectDolaVideoOption(ctx context.Context, triggerJS, option, name string) error {
	targetJSON, _ := json.Marshal(option)
	marker := "data-dola-select-" + name
	mark := fmt.Sprintf(`(() => {
	  const b = %s;
	  if (!b) return false;
	  const r = b.getBoundingClientRect();
	  if (r.width <= 0 || r.height <= 0) return false;
	  b.setAttribute(%q, "true"); return true;
	})()`, triggerJS, marker)
	if err := chromedp.Run(ctx,
		chromedp.Poll(mark, nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Click("["+marker+"]", chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("dola %s selector unavailable", name)
	}
	markOption := fmt.Sprintf(`(() => {
	  const target = %s;
	  const option = Array.from(document.querySelectorAll('[role="menuitem"],[role="option"]')).find(e => {
	    const r = e.getBoundingClientRect();
	    const text = e.textContent.trim();
	    return r.width > 0 && r.height > 0 && (text === target || text.startsWith(target+"Best") || text.startsWith(target+"\n"));
	  });
	  if (!option || option.getAttribute("aria-disabled") === "true" || option.hasAttribute("data-disabled")) return false;
	  option.setAttribute("data-dola-menu-option", "true"); return true;
	})()`, targetJSON)
	if err := chromedp.Run(ctx,
		chromedp.Poll(markOption, nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Click("[data-dola-menu-option]", chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("dola %s option %s unavailable", name, option)
	}
	return nil
}

func chooseDolaDuration(ctx context.Context, duration int) error {
	return selectDolaVideoOption(ctx,
		`Array.from(document.querySelectorAll("button")).find(b => /^\d+s$/.test(b.textContent.trim()))`,
		fmt.Sprintf("%ds", duration), "duration")
}

func navigateDolaPage(ctx context.Context, destination string) error {
	var durationHookInstalled bool
	if err := chromedp.Run(ctx, chromedp.Evaluate("!!window.__dolaDuration30", &durationHookInstalled)); err != nil {
		return err
	}
	if !durationHookInstalled {
		if err := chromedp.Run(ctx, chromedp.ActionFunc(func(actionCtx context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(dolaDuration30Script).Do(actionCtx)
			return err
		}), chromedp.Evaluate(dolaDuration30Script, nil)); err != nil {
			return err
		}
	}
	var current string
	if err := chromedp.Run(ctx, chromedp.Evaluate("location.href", &current)); err == nil && durationHookInstalled && strings.TrimRight(current, "/") == strings.TrimRight(destination, "/") {
		return nil
	}
	// Dispatch navigation without waiting for every external resource to finish.
	// waitDolaDocument and the video controls are the readiness authorities.
	return chromedp.Run(ctx, chromedp.ActionFunc(func(actionCtx context.Context) error {
		_, _, navigationError, _, err := page.Navigate(destination).Do(actionCtx)
		if err != nil {
			return err
		}
		if navigationError != "" {
			return errors.New("dola navigation failed")
		}
		return nil
	}))
}

func dolaBrowserOptions(ctx context.Context, path, proxyURL, profileDir string) ([]chromedp.ExecAllocatorOption, *proxybridge.Bridge, error) {
	if profileDir != "" {
		if err := os.MkdirAll(profileDir, 0700); err != nil {
			return nil, nil, err
		}
	}
	browserProxy := strings.TrimSpace(proxyURL)
	var bridge *proxybridge.Bridge
	if browserProxy != "" {
		parsed, err := url.Parse(browserProxy)
		if err != nil || parsed.Host == "" {
			return nil, nil, errors.New("invalid Dola proxy")
		}
		if parsed.User != nil {
			bridge, err = proxybridge.Start(ctx, browserProxy)
			if err != nil {
				return nil, nil, err
			}
			browserProxy = bridge.URL()
		}
	}
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	// Persist the account-specific profile so import validation and generation
	// reuse the same server-created browser state.
	opts = append(opts,
		chromedp.ExecPath(path),
		chromedp.Flag("headless", "new"),
		chromedp.WindowSize(1280, 900),
		chromedp.Flag("force-device-scale-factor", "1"),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-features", "Translate,BlinkGenPropertyTrees"),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
	)
	if profileDir != "" {
		opts = append(opts, chromedp.UserDataDir(profileDir))
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("OREATE_NO_SANDBOX")), "true") {
		opts = append(opts, chromedp.Flag("no-sandbox", true))
	}
	if browserProxy != "" {
		opts = append(opts, chromedp.ProxyServer(browserProxy))
	}
	if bridge != nil {
		opts = append(opts, chromedp.Flag("proxy-bypass-list", "<-loopback>"))
	}
	return opts, bridge, nil
}

func dolaCookieActions(account Account) []chromedp.Action {
	return []chromedp.Action{network.Enable(), chromedp.ActionFunc(func(ctx context.Context) error {
		for _, part := range strings.Split(account.Cookie, ";") {
			name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok || name == "" || !dolaCookieAllowed(name) {
				continue
			}
			// URL semantics are portable across Chromium versions and let the browser
			// derive a valid host scope even while the initial target is about:blank.
			err := network.SetCookie(name, value).WithURL(apiBase + "/").WithPath("/").WithSecure(true).Do(ctx)
			if err != nil && dolaCookieRequired(name) {
				return &cookieInstallError{name: name}
			}
		}
		return nil
	})}
}

type cookieInstallError struct{ name string }

func (err *cookieInstallError) Error() string { return "required Dola cookie rejected: " + err.name }

func dolaCookieRequired(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "sessionid", "sessionid_ss", "s_v_web_id":
		return true
	default:
		return false
	}
}

func dolaCookieAllowed(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "oauth_token", "oauth_token_v2", "sessionid", "sessionid_ss", "sid_tt", "sid_guard", "sid_ucp_v1", "ssid_ucp_v1", "s_v_web_id", "mstoken", "ttwid", "odin_tt", "uid_tt", "uid_tt_ss", "passport_auth_status", "passport_auth_status_ss", "passport_csrf_token", "passport_csrf_token_default", "passport_csrf_token_wap_state", "flow_user_country", "flow_cur_user_sec_id", "flow_multi_user_sec_info", "store-idc", "store-country-code", "store-country-code-src", "has_biz_token", "i18next", "conversation_list_v2_group_mode", "biz_trace_id", "hook_slardar_session_id", "dbx-web-theme":
		return true
	default:
		return false
	}
}

func browserDolaCookieHeader(ctx context.Context) (string, error) {
	var cookies []*network.Cookie
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(actionCtx context.Context) error {
		var err error
		cookies, err = network.GetCookies().WithURLs([]string{apiBase}).Do(actionCtx)
		return err
	}))
	if err != nil {
		return "", err
	}
	values := make(map[string]string)
	for _, cookie := range cookies {
		domain := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(cookie.Domain)), ".")
		if domain != "dola.com" && !strings.HasSuffix(domain, ".dola.com") {
			continue
		}
		values[cookie.Name] = cookie.Value
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+values[name])
	}
	return strings.Join(parts, "; "), nil
}

func dolaChromiumExecPath() string {
	for _, key := range []string{"DOLA_CHROME", "OREATE_CHROME"} {
		if configured := strings.TrimSpace(os.Getenv(key)); configured != "" {
			return configured
		}
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}

// Adds only duration options to live official configuration; model choices stay intact.
const dolaDuration30Script = `(() => {
  if (window.__dolaDuration30) return;
  window.__dolaDuration30 = true;
  const matches = input => {
    try {
      const u = new URL(typeof input === "string" ? input : input.url, location.href);
      return u.origin === "https://www.dola.com" &&
        (u.pathname.startsWith("/alice/slot/action_bar") || u.pathname === "/samantha/skill/pack");
    } catch { return false; }
  };
  function patch(value) {
    if (!value || typeof value !== "object") return;
    if (!Array.isArray(value)) {
      for (const key of ["option_list", "options"]) {
        const options = value[key];
        const label = String(value.value || value.label || value.name || value.title || "");
        if (Array.isArray(options) &&
            (/duration|时长/i.test(label) || options.some(o => o && /^(5|10)s$/.test(o.display_text || o.show_name || ""))) &&
            !options.some(o => o && String(o.option_key || o.value) === "30")) {
          const sample = options.find(o => o && String(o.option_key || o.value) === "10") || options[0];
          if (sample) options.push({...sample,
            id: Math.max(0, ...options.map(o => Number(o.id) || 0)) + 1,
            display_text: "30s", show_name: "30s", option_key: "30", value: "30", is_default: false});
        }
      }
      if (Array.isArray(value.supported_durations) && !value.supported_durations.some(d => String(d) === "30")) {
        value.supported_durations.push("30");
      }
    }
    for (const key of Object.keys(value)) {
      const child = value[key];
      if (typeof child === "string" && /^[\[{]/.test(child.trim())) {
        try { const parsed = JSON.parse(child); patch(parsed); value[key] = JSON.stringify(parsed); } catch {}
      } else patch(child);
    }
  }
  function transform(raw) {
    try { const value = JSON.parse(raw); patch(value); return JSON.stringify(value); } catch { return raw; }
  }
  const originalFetch = window.fetch;
  window.fetch = async function(input, init) {
    const response = await originalFetch.apply(this, arguments);
    if (!matches(input) || !response.ok) return response;
    try {
      const raw = await response.clone().text();
      const patched = transform(raw);
      if (patched === raw) return response;
      const headers = new Headers(response.headers);
      headers.delete("content-length"); headers.delete("content-encoding");
      return new Response(patched, {status: response.status, statusText: response.statusText, headers});
    } catch { return response; }
  };
  const originalOpen = XMLHttpRequest.prototype.open;
  XMLHttpRequest.prototype.open = function(method, url) {
    if (matches(url)) {
      const xhr = this;
      const listener = () => {
        if (xhr.readyState !== 4) return;
        xhr.removeEventListener("readystatechange", listener);
        if (xhr.status < 200 || xhr.status >= 300) return;
        try {
          if (xhr.responseType === "json") {
            const value = xhr.response; patch(value);
            Object.defineProperty(xhr, "response", {configurable: true, value});
          } else if (!xhr.responseType || xhr.responseType === "text") {
            const value = transform(xhr.responseText);
            Object.defineProperty(xhr, "responseText", {configurable: true, value});
            Object.defineProperty(xhr, "response", {configurable: true, value});
          }
        } catch {}
      };
      xhr.addEventListener("readystatechange", listener);
    }
    return originalOpen.apply(this, arguments);
  };
})();`
