// Package krea implements the Krea.ai (krea.ai) provider client. The durable
// credential is the browser cookie (Supabase "sb-superb-auth-token"); Krea's own
// Next.js backend reads it directly, so quota and generation just forward the
// cookie — there's no separate token-exchange step. tls-client gives a Chrome
// JA3/JA4 so Krea's Cloudflare edge doesn't flag the requests.
package krea

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// kreaAnonKey is Krea's public Supabase anon key (fixed, embedded in their
// frontend) — required as the apikey/bearer when refreshing a session token.
const kreaAnonKey = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJyb2xlIjoiYW5vbiIsImlzcyI6InN1cGFiYXNlIiwiaWF0IjoxNzc1Mjc4ODU3LCJleHAiOjE5MzI5NTg4NTd9.NUiqEOd__QsCCMjo3D1zrCAda5dLV2F5p6Kf584sZKc"

const (
	apiBase   = "https://www.krea.ai"
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

var (
	ErrAuth              = errors.New("krea auth failed")
	ErrQuotaExhausted    = errors.New("krea quota exhausted")
	ErrTemporaryUpstream = errors.New("krea upstream temporary error")
)

// refreshLeadSeconds renews the access token this many seconds BEFORE it expires
// (not lazily at expiry). The maintenance sweep refreshes proactively, so a
// dormant account's token never lapses — once the rotating refresh_token is gone
// (expired/consumed) the account can't recover, so we keep it perpetually fresh.
const refreshLeadSeconds = 600 // 10 minutes

type Client struct {
	proxyMu sync.RWMutex
	proxy   string
	// freshest cookie per account (key: user id) + a per-account refresh lock, so
	// concurrent callers don't each spend the single-use (rotating) refresh_token —
	// the first refreshes, the rest reuse the cached fresh cookie.
	mu      sync.Mutex
	cookies map[string]string
	locks   map[string]*sync.Mutex
	// actAt = last /app activation time per account (key: user id); actLocks gives
	// a per-account lock so concurrent generations wait for the first to finish the
	// (once-per-daily-reset) activation instead of each loading /app.
	actAt    map[string]int64
	actLocks map[string]*sync.Mutex
}

func NewClient(proxy string) *Client {
	return &Client{
		proxy:    strings.TrimSpace(proxy),
		cookies:  map[string]string{},
		locks:    map[string]*sync.Mutex{},
		actAt:    map[string]int64{},
		actLocks: map[string]*sync.Mutex{},
	}
}

func (c *Client) userLock(userID string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.locks[userID]
	if !ok {
		m = &sync.Mutex{}
		c.locks[userID] = m
	}
	return m
}

// RefreshIfNeeded returns a cookie whose access_token is still valid: if the
// stored one is (near) expired it spends the refresh_token to mint a new session
// and rebuilds the cookie. Returns (cookie, changed, err); changed=true means the
// caller must persist the new cookie (the refresh_token rotated). ErrAuth means
// the refresh_token is dead → the account is gone.
func (c *Client) RefreshIfNeeded(ctx context.Context, cookie string) (string, bool, error) {
	authVal := authCookieValue(cookie)
	sess, ok := decodeSession(authVal)
	if !ok {
		return cookie, false, nil // unparseable — let the downstream call surface the error
	}
	userID := nestedStr(sess, "user", "id")
	now := time.Now().Unix()

	lk := c.userLock(userID)
	lk.Lock()
	defer lk.Unlock()

	// A concurrent caller may already have refreshed this account.
	if userID != "" {
		c.mu.Lock()
		cached := c.cookies[userID]
		c.mu.Unlock()
		if cs, ok := decodeSession(authCookieValue(cached)); ok && toInt64(cs["expires_at"])-refreshLeadSeconds > now {
			return cached, cached != cookie, nil
		}
	}
	if toInt64(sess["expires_at"])-refreshLeadSeconds > now {
		return cookie, false, nil // still valid
	}
	refreshTok := strings.TrimSpace(stringValue(sess["refresh_token"]))
	if refreshTok == "" {
		return "", false, ErrAuth
	}

	respBody, status, err := c.refreshPost(ctx, refreshTok)
	if err != nil {
		return "", false, fmt.Errorf("%w: refresh: %s", ErrTemporaryUpstream, err.Error())
	}
	if status == 400 || status == 401 || status == 403 {
		return "", false, ErrAuth
	}
	if status != 200 {
		return "", false, fmt.Errorf("%w: refresh http %d: %s", ErrTemporaryUpstream, status, clip(respBody, 120))
	}
	var ns map[string]any
	if json.Unmarshal(respBody, &ns) != nil || strings.TrimSpace(stringValue(ns["access_token"])) == "" {
		return "", false, ErrAuth
	}
	newCookie := replaceAuthCookie(cookie, "base64-"+base64.StdEncoding.EncodeToString(respBody))
	if userID != "" {
		c.mu.Lock()
		c.cookies[userID] = newCookie
		c.mu.Unlock()
	}
	return newCookie, true, nil
}

func (c *Client) refreshPost(ctx context.Context, refreshToken string) ([]byte, int, error) {
	client, err := c.newProxyTLSClient()
	if err != nil {
		return nil, 0, err
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": refreshToken})
	req, err := http.NewRequest(http.MethodPost, apiBase+"/auth/v1/token?grant_type=refresh_token", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req = req.WithContext(ctx)
	req.Header = http.Header{
		"accept":                 {"*/*"},
		"content-type":           {"application/json;charset=UTF-8"},
		"apikey":                 {kreaAnonKey},
		"authorization":          {"Bearer " + kreaAnonKey},
		"x-client-info":          {"supabase-ssr/0.6.1 createBrowserClient"},
		"x-supabase-api-version": {"2024-01-01"},
		"origin":                 {apiBase},
		"referer":                {apiBase + "/"},
		"user-agent":             {userAgent},
		http.HeaderOrderKey: {
			"accept", "content-type", "apikey", "authorization", "x-client-info",
			"x-supabase-api-version", "origin", "referer", "user-agent",
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

// chunkSize is supabase-ssr's per-cookie chunk limit; larger sessions (e.g.
// Google-OAuth accounts) are split into sb-superb-auth-token.0/.1/...
const chunkSize = 3600

func cookieVal(cookie, name string) string {
	for _, p := range strings.Split(cookie, ";") {
		p = strings.TrimSpace(p)
		if v, ok := strings.CutPrefix(p, name+"="); ok {
			return v
		}
	}
	return ""
}

// authCookieValue returns the full auth value, transparently reassembling a
// chunked cookie (sb-superb-auth-token.0 + .1 + ...) or returning the single one.
func authCookieValue(cookie string) string {
	if v := cookieVal(cookie, "sb-superb-auth-token"); v != "" {
		return v
	}
	var b strings.Builder
	for i := 0; ; i++ {
		v := cookieVal(cookie, fmt.Sprintf("sb-superb-auth-token.%d", i))
		if v == "" {
			break
		}
		b.WriteString(v)
	}
	return b.String()
}

// replaceAuthCookie drops every sb-superb-auth-token[.N] cookie and re-adds the
// new value (chunked the same way supabase-ssr would if it's large), preserving
// all other cookies (krea-workspace-id, etc.).
func replaceAuthCookie(cookie, newValue string) string {
	var out []string
	for _, p := range strings.Split(cookie, ";") {
		t := strings.TrimSpace(p)
		if t == "" || strings.HasPrefix(t, "sb-superb-auth-token=") || strings.HasPrefix(t, "sb-superb-auth-token.") {
			continue
		}
		out = append(out, t)
	}
	if len(newValue) <= chunkSize {
		out = append(out, "sb-superb-auth-token="+newValue)
	} else {
		for i, off := 0, 0; off < len(newValue); i++ {
			end := off + chunkSize
			if end > len(newValue) {
				end = len(newValue)
			}
			out = append(out, fmt.Sprintf("sb-superb-auth-token.%d=%s", i, newValue[off:end]))
			off = end
		}
	}
	return strings.Join(out, "; ")
}

// decodeSession base64-decodes the auth cookie value into the session JSON map.
func decodeSession(authValue string) (map[string]any, bool) {
	v := strings.TrimPrefix(strings.TrimSpace(authValue), "base64-")
	if v == "" {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(v)
		if err != nil {
			return nil, false
		}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil, false
	}
	return m, true
}

func nestedStr(m map[string]any, k1, k2 string) string {
	if sub, ok := m[k1].(map[string]any); ok {
		return strings.TrimSpace(stringValue(sub[k2]))
	}
	return ""
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	default:
		return 0
	}
}

func (c *Client) proxyValue() string {
	c.proxyMu.RLock()
	defer c.proxyMu.RUnlock()
	return c.proxy
}

// IsKreaCookie reports whether a pasted credential is a Krea cookie: it carries
// the Supabase auth cookie. Distinguishes it from adobe/leonardo cookies.
func IsKreaCookie(value string) bool {
	return strings.Contains(value, "sb-superb-auth-token")
}

// EmailFromCookie decodes the account email straight out of the cookie's embedded
// Supabase session (no network), handling chunked cookies too.
func EmailFromCookie(cookie string) string {
	sess, ok := decodeSession(authCookieValue(cookie))
	if !ok {
		return ""
	}
	return nestedStr(sess, "user", "email")
}

// FetchCreditsBalance reads the account's free-credit balance via /api/billing-data.
// remaining is the integer floor of balance.free (per spec: 17.94 → 17). 401 →
// ErrAuth (cookie dead). Returns the normalized map shared by all providers.
func (c *Client) FetchCreditsBalance(ctx context.Context, cookie string) (map[string]any, error) {
	// Detach from the request ctx so a page refresh can't cancel the probe
	// mid-flight (which left accounts stuck at "—").
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	// NOTE: no /app here — the heavy SSR activation is done separately (on recovery
	// and at generation via Activate). This probe just reads the current balance.
	body, status, err := c.apiGetP(probeCtx, cookie, "/api/billing-data", true)
	if err != nil {
		return unknownBalance("network: " + err.Error()), nil
	}
	if status == 401 || status == 403 {
		return nil, ErrAuth
	}
	if status != 200 {
		return unknownBalance(fmt.Sprintf("http %d: %s", status, clip(body, 160))), nil
	}
	// 余额在 balance.free(真实剩余,小数,随用量递减)。krea 的这个字段一直都在,
	// 只是排在很长的 entitlements 之后 —— 只读它,不用套餐配额兜底。
	var bd struct {
		Balance struct {
			Free  float64 `json:"free"`
			Total float64 `json:"total"`
		} `json:"balance"`
	}
	if err := json.Unmarshal(body, &bd); err != nil {
		return unknownBalance("non-json"), nil
	}
	remaining := int(bd.Balance.Free) // floor
	return map[string]any{
		"remaining": remaining,
		"used":      nil,
		"total":     int(bd.Balance.Total),
		"unknown":   false,
		"error":     nil,
		"email":     emptyStringNil(EmailFromCookie(cookie)),
	}, nil
}

// Activate loads the authenticated SSR app page (/app), which is what makes krea
// grant the account's DAILY free balance — a cold API-only call (billing-data /
// generate) otherwise sees balance.free=0 and 402s. Done at most ONCE per account
// per daily reset, under a per-account lock: the first caller loads /app while
// concurrent callers wait, then everyone proceeds (no redundant /app). Called
// before each generation and by the daily activation sweep. Best-effort.
func (c *Client) Activate(ctx context.Context, cookie string) {
	key := accountKey(cookie)
	lastReset := (time.Now().Unix() / 86400) * 86400
	c.mu.Lock()
	doneToday := key != "" && c.actAt[key] >= lastReset
	c.mu.Unlock()
	if doneToday {
		return
	}
	lk := c.actLock(key)
	lk.Lock()
	defer lk.Unlock()
	// Re-check after acquiring the lock — another caller may have just activated.
	c.mu.Lock()
	doneToday = key != "" && c.actAt[key] >= lastReset
	c.mu.Unlock()
	if doneToday {
		return
	}
	actCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	_, _, _ = c.apiGetP(actCtx, cookie, "/app", true)
	c.mu.Lock()
	c.actAt[key] = time.Now().Unix()
	c.mu.Unlock()
}

func (c *Client) actLock(key string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.actLocks[key]
	if !ok {
		m = &sync.Mutex{}
		c.actLocks[key] = m
	}
	return m
}

// accountKey is the stable per-account id (the Supabase user id from the session
// cookie) used to key activation state — survives cookie rotation.
func accountKey(cookie string) string {
	if sess, ok := decodeSession(authCookieValue(cookie)); ok {
		return nestedStr(sess, "user", "id")
	}
	return ""
}

// apiGetP keeps the route explicit: authentication/quota/bootstrap callers use
// the proxy, while project setup and polling can remain direct.
func (c *Client) apiGetP(ctx context.Context, cookie, path string, useProxy bool) ([]byte, int, error) {
	client, err := c.newTLSClientP(useProxy)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest(http.MethodGet, apiBase+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req = req.WithContext(ctx)
	req.Header = http.Header{
		"accept":          {"*/*"},
		"accept-language": {"en-US,en;q=0.9"},
		"cookie":          {cookie},
		"referer":         {apiBase + "/"},
		"user-agent":      {userAgent},
		"sec-fetch-dest":  {"empty"},
		"sec-fetch-mode":  {"cors"},
		"sec-fetch-site":  {"same-origin"},
		http.HeaderOrderKey: {
			"accept", "accept-language", "cookie", "referer", "user-agent",
			"sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site",
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

// newProxyTLSClient is used for authentication/account maintenance and submit.
func (c *Client) newProxyTLSClient() (tlsclient.HttpClient, error) { return c.newTLSClientP(true) }

// newDirectTLSClient is used for project setup, media, polling, and downloads.
func (c *Client) newDirectTLSClient() (tlsclient.HttpClient, error) { return c.newTLSClientP(false) }

func (c *Client) newTLSClientP(useProxy bool) (tlsclient.HttpClient, error) {
	options := []tlsclient.HttpClientOption{
		tlsclient.WithTimeoutSeconds(60),
		tlsclient.WithClientProfile(profiles.Chrome_120),
	}
	if useProxy {
		if proxy := c.proxyValue(); proxy != "" {
			options = append(options, tlsclient.WithProxyUrl(proxy))
		}
	}
	return tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), options...)
}

func unknownBalance(reason string) map[string]any {
	return map[string]any{
		"remaining": nil, "used": nil, "total": nil, "unknown": true, "error": reason,
	}
}

func stringValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		b, _ := json.Marshal(x)
		return strings.TrimSpace(string(b))
	}
}

func emptyStringNil(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

func clip(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n]
	}
	return s
}
