// Package dola implements the Dola (Doubao international, dola.com) website
// provider. Dola does not expose a public API: generation goes through the
// website chat transport, authenticated by the browser cookie, and video
// results are polled back from the conversation chain.
package dola

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"backend/internal/provider/proxypool"

	"github.com/google/uuid"
)

const (
	apiBase      = "https://www.dola.com"
	dolaAID      = "495671"
	dolaBotID    = "7339470689562525703"
	versionCode  = "20800"
	defaultUA    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	maxCookieLen = 64 << 10
)

var (
	ErrAuth              = errors.New("dola auth failed")
	ErrContentRejected   = errors.New("dola content rejected")
	ErrQuotaExhausted    = errors.New("dola quota exhausted")
	ErrTemporaryUpstream = errors.New("dola upstream temporary error")
	ErrChallenge         = fmt.Errorf("%w: browser verification required", ErrTemporaryUpstream)
)

// Account carries one imported Dola browser session. The cookie is the
// credential; SessionID/FP/MSToken are derived from it on normalization.
type Account struct {
	ID            string `json:"-"`
	Cookie        string
	UserAgent     string
	SessionID     string
	FP            string
	MSToken       string
	ProtocolQuery url.Values `json:"-"`
	ProtocolProxy string     `json:"-"`
}

func (a Account) normalized() Account {
	a.ID = strings.TrimSpace(a.ID)
	a.Cookie = strings.TrimSpace(a.Cookie)
	a.UserAgent = strings.TrimSpace(a.UserAgent)
	if a.UserAgent == "" {
		a.UserAgent = defaultUA
	}
	if a.SessionID == "" {
		a.SessionID = CookieValue(a.Cookie, "sessionid")
	}
	if a.FP == "" {
		a.FP = CookieValue(a.Cookie, "s_v_web_id")
	}
	if a.MSToken == "" {
		a.MSToken = CookieValue(a.Cookie, "msToken")
	}
	return a
}

// CookieValue extracts one cookie pair from a header-style cookie string.
func CookieValue(cookie, name string) string {
	for _, part := range strings.Split(cookie, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.TrimSpace(key) == name {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// IsDolaCookie recognizes full browser exports and Passport login-only bundles.
// This is a routing hint; only the Dola authentication probe proves validity.
func IsDolaCookie(cookie string) bool {
	if len(cookie) > maxCookieLen {
		return false
	}
	session := CookieValue(cookie, "sessionid")
	if session == "" {
		return false
	}
	if CookieValue(cookie, "s_v_web_id") != "" {
		return true
	}
	return CookieValue(cookie, "sid_tt") == session && CookieValue(cookie, "sid_guard") != "" &&
		(CookieValue(cookie, "passport_csrf_token") != "" || CookieValue(cookie, "passport_auth_status") != "") &&
		(CookieValue(cookie, "store-idc") != "" || CookieValue(cookie, "store-country-code") != "")
}

// IdentityHash derives the stable account identity from the session. The
// sessionid survives page reloads and is the only durable per-account value
// in the jar.
func IdentityHash(sessionID string) string {
	sum := sha256.Sum256([]byte("dola:" + strings.TrimSpace(sessionID)))
	return hex.EncodeToString(sum[:])
}

type Client struct {
	proxyMu            sync.RWMutex
	proxy              string
	assigner           proxypool.Assigner
	baseURL            string
	directClient       *http.Client
	directClientProxy  string
	protocolIdentities sync.Map // session identity -> *protocolIdentity

	accountClients   map[string]*http.Client
	browserMu        sync.Mutex
	boundBrowsers    map[string]*boundBrowser
	portableBrowsers map[string]*portableBrowser
}

func NewClient(proxy string) *Client {
	return &Client{
		proxy:          strings.TrimSpace(proxy),
		baseURL:        apiBase,
		accountClients: make(map[string]*http.Client),
	}
}

func (c *Client) SetProxy(proxy string) {
	proxy = strings.TrimSpace(proxy)
	c.proxyMu.Lock()
	defer c.proxyMu.Unlock()
	if c.proxy == proxy {
		return
	}
	c.proxy = proxy
	c.invalidateClientsLocked(true)
}

func (c *Client) SetAssigner(assigner proxypool.Assigner) {
	c.proxyMu.Lock()
	c.assigner = assigner
	c.proxyMu.Unlock()
}

func (c *Client) RotateProxySession(accountID string) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return
	}
	if assigner := c.assignerValue(); assigner != nil {
		assigner.Rotate(accountID)
	}
	c.proxyMu.Lock()
	defer c.proxyMu.Unlock()
	for key, client := range c.accountClients {
		if key == accountID || strings.HasPrefix(key, accountID+"|") {
			if client != nil {
				if transport, ok := client.Transport.(*http.Transport); ok {
					transport.CloseIdleConnections()
				}
			}
			delete(c.accountClients, key)
		}
	}
}

func (c *Client) assignerValue() proxypool.Assigner {
	c.proxyMu.RLock()
	defer c.proxyMu.RUnlock()
	return c.assigner
}

func (c *Client) invalidateClientsLocked(clearProxies bool) {
	if c.directClient != nil {
		if transport, ok := c.directClient.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
	for _, client := range c.accountClients {
		if client == nil {
			continue
		}
		if transport, ok := client.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
	c.directClient = nil
	c.directClientProxy = ""
	c.accountClients = make(map[string]*http.Client)
}

// AccountProxy resolves the sticky (or shared fallback) proxy used for every
// request belonging to one Dola account. Empty is an error because Dola must
// never silently use the server's direct egress.
func (c *Client) browserLock() *sync.Mutex {
	return &c.browserMu
}

func profileVersion(cookie string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(cookie)))
	return hex.EncodeToString(sum[:])[:16]
}

func dolaProfileDir(account Account) string {
	root := strings.TrimSpace(os.Getenv("DOLA_PROFILE_DIR"))
	if root == "" {
		root = "/app/data/dola-profiles"
	}
	identity := IdentityHash(CookieValue(account.Cookie, "sessionid"))
	return filepath.Join(root, identity+"-"+profileVersion(account.Cookie))
}

func (c *Client) AccountProxy(ctx context.Context, account Account) (string, error) {
	if account.ProtocolProxy != "" {
		return account.ProtocolProxy, nil
	}

	binding, err := loadBrowserBinding(account)
	if err != nil {
		return "", err
	}
	proxyURL := ""
	if binding != nil {
		proxyURL = binding.Proxy
	} else {
		proxyURL = strings.TrimSpace(c.assignAccountProxy(ctx, account))
	}
	if proxyURL == "" {
		return "", errors.New("dola proxy is not configured")
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Host == "" {
		return "", errors.New("dola proxy configuration is invalid")
	}
	return proxyURL, nil
}

func (c *Client) endpoint(path string) string {
	base := strings.TrimRight(strings.TrimSpace(c.baseURL), "/")
	if base == "" {
		base = apiBase
	}
	return base + path
}

// Dola is region-locked to JP/KR exits, so every API call leaves through the
// configured proxy when one exists; artifact downloads stay on direct egress.
// The client is rebuilt whenever the proxy changes so a late proxy.url update
// takes effect without a restart.
func (c *Client) httpClient() *http.Client {
	c.proxyMu.RLock()
	proxy := c.proxy
	cached := c.directClient
	cachedProxy := c.directClientProxy
	c.proxyMu.RUnlock()
	if cached != nil && cachedProxy == proxy {
		return cached
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if proxy != "" {
		if parsed, err := url.Parse(proxy); err == nil {
			transport.Proxy = http.ProxyURL(parsed)
		}
	}
	client := &http.Client{Transport: transport}
	c.proxyMu.Lock()
	c.directClient = client
	c.directClientProxy = proxy
	c.proxyMu.Unlock()
	return client
}

// accountClient returns the HTTP client pinned to one account's proxy exit.
// Dola is never allowed to use direct server egress: a sticky assignment may
// fall back only to the configured shared proxy, otherwise the request fails.
func (c *Client) accountClient(ctx context.Context, account Account) (*http.Client, error) {
	account = account.normalized()
	if strings.TrimRight(strings.TrimSpace(c.baseURL), "/") != apiBase {
		return c.httpClient(), nil
	}
	proxyURL, err := c.AccountProxy(ctx, account)
	if err != nil {
		return nil, err
	}
	clientKey := account.ID + "|" + proxyURL
	c.proxyMu.RLock()
	cached := c.accountClients[clientKey]
	c.proxyMu.RUnlock()
	if cached != nil {
		return cached, nil
	}
	parsed, _ := url.Parse(proxyURL)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(parsed)
	client := &http.Client{Transport: transport}
	c.proxyMu.Lock()
	c.accountClients[clientKey] = client
	c.proxyMu.Unlock()
	return client, nil
}

// assignAccountProxy resolves (and caches) the sticky exit for one account.
// It reads the provider's session API once per account and reuses the result.
func (c *Client) assignAccountProxy(ctx context.Context, account Account) string {
	account = account.normalized()
	id := strings.TrimSpace(account.ID)
	if id == "" {
		id = IdentityHash(account.SessionID)
	}
	if assigner := c.assignerValue(); assigner != nil {
		if proxyURL, ok := assigner.Assign(ctx, id, dolaAccountRegion(account)); ok {
			return proxyURL
		}
	}
	c.proxyMu.RLock()
	defer c.proxyMu.RUnlock()
	return c.proxy
}

func dolaAccountRegion(account Account) string {
	region := strings.ToUpper(strings.TrimSpace(CookieValue(account.Cookie, "flow_user_country")))
	switch region {
	case "JP", "KR", "SG", "HK":
		return region
	default:
		return "JP"
	}
}

// commonQuery mirrors the full parameter set the Dola webapp attaches to every
// samantha request, including the device-fingerprint fields the risk engine
// reads. msToken/fp come from the imported cookie; the device ids are stable
// per-install values the web client generates once and reuses.
func commonQuery(account Account) url.Values {
	return commonQueryForTab(account, uuid.NewString())
}

func commonQueryForTab(account Account, webTabID string) url.Values {
	if len(account.ProtocolQuery) > 0 {
		query := url.Values{}
		for k, v := range account.ProtocolQuery {
			query[k] = append([]string(nil), v...)
		}
		query.Set("web_tab_id", webTabID)
		return query
	}

	// Mirror the minimal parameter set the verified reference client sends for
	// the chat/completion (video) path. Do NOT add fabricated device_id / web_id
	// / tea_uuid fields: the browser's real device ids are paired with its
	// msToken, so unpaired fake ones fail risk control instead of passing it.
	query := url.Values{}
	query.Set("aid", dolaAID)
	query.Set("channel", "g")
	query.Set("device_platform", "web")
	query.Set("language", "zh-Hant")
	region := dolaAccountRegion(account)
	query.Set("region", region)
	query.Set("sys_region", region)
	query.Set("samantha_web", "1")
	query.Set("use-olympus-account", "1")
	query.Set("version_code", versionCode)
	query.Set("web_platform", "browser")
	webTabID = strings.TrimSpace(webTabID)
	if webTabID == "" {
		webTabID = uuid.NewString()
	}
	query.Set("web_tab_id", webTabID)
	if account.MSToken != "" {
		query.Set("msToken", account.MSToken)
	}
	if account.FP != "" {
		query.Set("fp", account.FP)
	}
	return query
}

func setHeaders(req *http.Request, account Account, contentType, agwConv, referer string) {
	account = account.normalized()
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en,ja;q=0.9,en-US;q=0.8,en;q=0.7")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Cookie", account.Cookie)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Origin", apiBase)
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Referer", referer)
	req.Header.Set("User-Agent", account.UserAgent)
	req.Header.Set("agw-js-conv", agwConv)
}

type chainEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// postJSON performs one cookie-authenticated JSON call through the proxy.
func (c *Client) postJSON(ctx context.Context, account Account, path string, query url.Values, body any, contentType, agwConv, referer string, limit int64) ([]byte, int, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(path)+"?"+query.Encode(), bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	setHeaders(req, account, contentType, agwConv, referer)
	client, err := c.accountClient(ctx, account)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrTemporaryUpstream, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: request failed: %v", ErrTemporaryUpstream, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}

// ProbeSession validates an imported cookie without submitting a generation.
// An auth failure is definitive; anything else leaves the session usable.
func (c *Client) ProbeSession(ctx context.Context, account Account) error {
	account = account.normalized()
	if account.Cookie == "" {
		return ErrAuth
	}
	body := map[string]any{"cmd": 3100, "conversation_id": "", "anchor_index": 0, "direction": 1, "limit": 1}
	data, status, err := c.postJSON(ctx, account, "/im/chain/single", commonQuery(account), body,
		"application/json; encoding=utf-8", "str", c.endpoint("/chat/"), 2<<20)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return ErrAuth
	}
	var envelope chainEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		// A 200 with a non-JSON body still proves the session survived the edge.
		if status == http.StatusOK {
			return nil
		}
		return fmt.Errorf("%w: invalid probe response", ErrTemporaryUpstream)
	}
	if envelope.Code != 0 && isAuthCode(envelope.Code) {
		return ErrAuth
	}
	return nil
}

// isAuthCode recognizes the samantha error codes that mean the cookie is no
// longer accepted, as opposed to bad parameters or upstream faults.
func isAuthCode(code int) bool {
	switch code {
	case 401, 403, 40001, 40003, 40101, 40103, 40105:
		return true
	}
	return false
}
