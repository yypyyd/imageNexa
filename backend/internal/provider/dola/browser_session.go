package dola

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/chromedp/cdproto/target"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

// Operator-owned file in the private profile volume; never populated from API input.
const browserBindingsPath = "/app/data/dola-profiles/browser-bindings.json"

type browserBinding struct {
	Target   string `json:"target_id"`
	Identity string `json:"identity_hash"`
	Endpoint string `json:"endpoint"`
	Proxy    string `json:"proxy"`
}

func loadBrowserBinding(account Account) (*browserBinding, error) {
	data, err := os.ReadFile(browserBindingsPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("dola browser binding unavailable")
	}
	return parseBrowserBinding(data, account)
}

func parseBrowserBinding(data []byte, account Account) (*browserBinding, error) {
	var bindings map[string]browserBinding
	if json.Unmarshal(data, &bindings) != nil {
		return nil, errors.New("dola browser binding invalid")
	}
	binding, ok := bindings[account.ID]
	if !ok {
		return nil, nil
	}
	if binding.Identity != IdentityHash(CookieValue(account.Cookie, "sessionid")) {
		return nil, errors.New("dola browser binding belongs to a different credential")
	}
	endpoint, err := url.Parse(binding.Endpoint)
	if err != nil || endpoint.Scheme != "ws" || endpoint.User != nil || !strings.HasPrefix(endpoint.Path, "/devtools/browser/") {
		return nil, errors.New("dola browser binding endpoint invalid")
	}
	ip := net.ParseIP(endpoint.Hostname())
	if ip == nil || (!ip.IsLoopback() && !ip.IsPrivate()) {
		return nil, errors.New("dola browser must use a private literal address")
	}
	proxy, err := url.Parse(binding.Proxy)
	if err != nil || proxy.Host == "" || (proxy.Scheme != "http" && proxy.Scheme != "https") {
		return nil, errors.New("dola browser binding proxy invalid")
	}
	return &binding, nil
}

// Access is serialized by Client.browserLock. Per-request cancellation does not
// destroy the persistent tab or its verified browser state.
type boundBrowser struct {
	binding browserBinding
	ctx     context.Context
	cancel  context.CancelFunc
}

func (c *Client) openBoundBrowser(ctx context.Context, account Account, binding browserBinding) (context.Context, func(), error) {
	if c.boundBrowsers == nil {
		c.boundBrowsers = make(map[string]*boundBrowser)
	}
	existing := c.boundBrowsers[account.ID]
	if existing != nil && (existing.binding != binding || existing.ctx.Err() != nil) {
		existing.cancel()
		delete(c.boundBrowsers, account.ID)
		existing = nil
	}
	if existing == nil {
		alloc, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), binding.Endpoint, chromedp.NoModifyURL)
		opts := []chromedp.ContextOption{}
		if binding.Target != "" {
			opts = append(opts, chromedp.WithTargetID(target.ID(binding.Target)))
		}
		tab, cancelTab := chromedp.NewContext(alloc, opts...)
		// Allocate on the persistent context, not a request child: RemoteAllocator
		// otherwise closes the browser connection when the first request finishes.
		stopInit := context.AfterFunc(ctx, cancelTab)
		initErr := chromedp.Run(tab)
		stopInit()
		if initErr != nil {
			cancelTab()
			cancelAlloc()
			return nil, func() {}, errors.New("dola bound browser connection failed")
		}
		existing = &boundBrowser{binding: binding, ctx: tab, cancel: func() { cancelTab(); cancelAlloc() }}
		c.boundBrowsers[account.ID] = existing
	}
	pageCtx, cancelPage := context.WithTimeout(existing.ctx, videoSubmitWait)
	stop := context.AfterFunc(ctx, cancelPage)
	cleanup := func() { stop(); cancelPage() }
	cookie, err := browserDolaCookieHeader(pageCtx)
	if err != nil || CookieValue(cookie, "sessionid") != CookieValue(account.Cookie, "sessionid") {
		cleanup()
		return nil, func() {}, ErrAuth
	}
	if err := navigateDolaPage(pageCtx, apiBase+"/chat/"); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err = waitDolaDocument(pageCtx); err == nil {
			break
		}
		if pageCtx.Err() != nil {
			break
		}
		_ = chromedp.Run(pageCtx, chromedp.Reload())
	}
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	if err = chromedp.Run(pageCtx, chromedp.Sleep(time.Second)); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return pageCtx, cleanup, nil
}
