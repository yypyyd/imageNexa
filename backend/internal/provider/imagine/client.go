// Package imagine implements the Imagine.art (vyro.ai) provider client. The
// durable credential is a JSON blob {"token","refreshToken"}: `token` is a ~6h
// access JWT used as Authorization: Bearer for the API, and `refreshToken` is a
// ~7d JWT that mints a fresh pair via /apis/v1/auth/other/refresh/web when the
// access token expires. Both rotate on refresh, so the new pair MUST be saved.
// tls-client gives a Chrome JA3/JA4 so vyro's edge doesn't flag the requests.
package imagine

import (
	"context"
	"crypto/rand"
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

const (
	apiBase   = "https://imagine.vyro.ai"
	teamsBase = "https://teams-imagine.vyro.ai"
	authBase  = "https://auth.vyro.ai"
	webOrigin = "https://www.imagine.art"
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

var (
	ErrAuth              = errors.New("imagine auth failed")
	ErrQuotaExhausted    = errors.New("imagine quota exhausted")
	ErrTemporaryUpstream = errors.New("imagine upstream temporary error")
)

// refreshLeadSeconds renews the access token this many seconds BEFORE it expires
// (proactive, not lazy at expiry) — the maintenance sweep keeps tokens fresh so a
// dormant account's rotating refreshToken never lapses.
const refreshLeadSeconds = 600 // 10 minutes

type Client struct {
	proxyMu sync.RWMutex
	proxy   string
	// freshest credential per account (key: user id) + a per-account refresh lock,
	// so concurrent callers don't each spend the rotating refresh_token — the first
	// refreshes, the rest reuse the cached fresh credential.
	mu    sync.Mutex
	creds map[string]string
	locks map[string]*sync.Mutex
}

func NewClient(proxy string) *Client {
	return &Client{proxy: strings.TrimSpace(proxy), creds: map[string]string{}, locks: map[string]*sync.Mutex{}}
}

func (c *Client) proxyValue() string {
	c.proxyMu.RLock()
	defer c.proxyMu.RUnlock()
	return c.proxy
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

// ---------------------------------------------------------------------------
// Credential helpers
// ---------------------------------------------------------------------------

type credential struct {
	Token        string `json:"token"`
	RefreshToken string `json:"refreshToken"`
	// Email is the real account email — supplied at import, used for display and
	// (pool,email) dedup. It is NOT in the JWT (which only carries userId), so it
	// must be carried across refreshes (the refresh response omits it).
	Email string `json:"email,omitempty"`
	// ParentID is a canvas node the account OWNS, used as the generation's
	// parent_id. Imagine rejects any parent the account doesn't own ("user does
	// not have access to parent asset") and silently orphans a parent-less
	// generation (charged but never produced) — so it's supplied at import and
	// carried across refreshes.
	ParentID string `json:"parentId,omitempty"`
}

func parseCred(s string) (credential, bool) {
	var cr credential
	if json.Unmarshal([]byte(strings.TrimSpace(s)), &cr) != nil {
		return cr, false
	}
	if strings.TrimSpace(cr.Token) == "" || strings.TrimSpace(cr.RefreshToken) == "" {
		return cr, false
	}
	return cr, true
}

func buildCred(token, refresh, email, parentID string) string {
	b, _ := json.Marshal(credential{
		Token:        strings.TrimSpace(token),
		RefreshToken: strings.TrimSpace(refresh),
		Email:        strings.TrimSpace(email),
		ParentID:     strings.TrimSpace(parentID),
	})
	return string(b)
}

func looksLikeJWT(s string) bool {
	return len(strings.Split(strings.TrimSpace(s), ".")) == 3
}

// IsImagineToken reports whether a pasted credential is an Imagine.art account:
// a JSON object carrying a non-empty token + refreshToken that both look like
// JWTs. Distinguishes it from adobe/leonardo/krea cookies.
func IsImagineToken(value string) bool {
	cr, ok := parseCred(value)
	if !ok {
		return false
	}
	return looksLikeJWT(cr.Token) && looksLikeJWT(cr.RefreshToken)
}

// jwtClaims base64url-decodes the JWT payload (segment 1) into a claims map.
func jwtClaims(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// tolerate padded variants
		if raw, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return nil
		}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

func userIDFromToken(token string) string {
	claims := jwtClaims(token)
	if claims == nil {
		return ""
	}
	if v := strings.TrimSpace(stringValue(claims["userId"])); v != "" {
		return v
	}
	return strings.TrimSpace(stringValue(claims["sub"]))
}

func tokenExp(token string) int64 {
	claims := jwtClaims(token)
	if claims == nil {
		return 0
	}
	return toInt64(claims["exp"])
}

// EmailFromCred returns the real account email supplied at import; if absent it
// falls back to the JWT userId so (pool,email) dedup still has a stable key.
func EmailFromCred(cred string) string {
	cr, ok := parseCred(cred)
	if !ok {
		return ""
	}
	if e := strings.TrimSpace(cr.Email); e != "" {
		return e
	}
	return userIDFromToken(cr.Token)
}

// ---------------------------------------------------------------------------
// Refresh
// ---------------------------------------------------------------------------

// RefreshIfNeeded returns a credential whose access token is still valid: if the
// stored one is (near) expired it spends the refreshToken to mint a fresh pair
// and rebuilds the credential. Returns (cred, changed, err); changed=true means
// the caller must persist the new credential (both tokens rotate). ErrAuth means
// the refreshToken is dead → the account is gone.
func (c *Client) RefreshIfNeeded(ctx context.Context, cred string) (string, bool, error) {
	cr, ok := parseCred(cred)
	if !ok {
		return cred, false, nil // unparseable — let the downstream call surface the error
	}
	userID := userIDFromToken(cr.Token)
	now := time.Now().Unix()

	lk := c.userLock(userID)
	lk.Lock()
	defer lk.Unlock()

	// A concurrent caller may already have refreshed this account.
	if userID != "" {
		c.mu.Lock()
		cached := c.creds[userID]
		c.mu.Unlock()
		if cc, ok := parseCred(cached); ok && tokenExp(cc.Token)-refreshLeadSeconds > now {
			return cached, cached != cred, nil
		}
	}
	if tokenExp(cr.Token)-refreshLeadSeconds > now {
		return cred, false, nil // still valid
	}

	respBody, status, err := c.refreshPost(ctx, cr.RefreshToken)
	if err != nil {
		return "", false, fmt.Errorf("%w: refresh: %s", ErrTemporaryUpstream, err.Error())
	}
	if status == 400 || status == 401 || status == 403 {
		return "", false, ErrAuth
	}
	if status != 200 {
		return "", false, fmt.Errorf("%w: refresh http %d: %s", ErrTemporaryUpstream, status, clip(respBody, 120))
	}
	var rb struct {
		Result struct {
			SessionToken string `json:"sessionToken"`
			RefreshToken string `json:"refreshToken"`
		} `json:"result"`
	}
	if json.Unmarshal(respBody, &rb) != nil || strings.TrimSpace(rb.Result.SessionToken) == "" {
		return "", false, ErrAuth
	}
	newRefresh := rb.Result.RefreshToken
	if strings.TrimSpace(newRefresh) == "" {
		newRefresh = cr.RefreshToken // some responses may omit it — keep the old one
	}
	newCred := buildCred(rb.Result.SessionToken, newRefresh, cr.Email, cr.ParentID)
	if userID != "" {
		c.mu.Lock()
		c.creds[userID] = newCred
		c.mu.Unlock()
	}
	return newCred, true, nil
}

func (c *Client) refreshPost(ctx context.Context, refreshToken string) ([]byte, int, error) {
	client, err := c.newProxyTLSClient()
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest(http.MethodPost, authBase+"/apis/v1/auth/other/refresh/web", nil)
	if err != nil {
		return nil, 0, err
	}
	req = req.WithContext(ctx)
	req.Header = http.Header{
		"accept":        {"application/json, text/plain, */*"},
		"authorization": {"Bearer " + refreshToken},
		"origin":        {webOrigin},
		"referer":       {webOrigin + "/"},
		"user-agent":    {userAgent},
		http.HeaderOrderKey: {
			"accept", "authorization", "origin", "referer", "user-agent",
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

// ---------------------------------------------------------------------------
// Credits
// ---------------------------------------------------------------------------

// FetchCreditsBalance reads the account's credit balance via /v1/credit.
// remaining is the `total` field. 401/403 → ErrAuth (token dead). Returns the
// normalized map shared by all providers.
func (c *Client) FetchCreditsBalance(ctx context.Context, cred string) (map[string]any, error) {
	cr, ok := parseCred(cred)
	if !ok {
		return unknownBalance("bad credential"), nil
	}
	userID := userIDFromToken(cr.Token)
	body, status, err := c.apiGetP(ctx, cr.Token, apiBase+"/v1/credit?org_id="+userID, true)
	if err != nil {
		return unknownBalance("network: " + err.Error()), nil
	}
	if status == 401 || status == 403 {
		return nil, ErrAuth
	}
	if status != 200 {
		return unknownBalance(fmt.Sprintf("http %d: %s", status, clip(body, 160))), nil
	}
	var cb struct {
		Status string `json:"status"`
		Total  int    `json:"total"`
	}
	if err := json.Unmarshal(body, &cb); err != nil {
		return unknownBalance("non-json"), nil
	}
	return map[string]any{
		"remaining": cb.Total,
		"used":      nil,
		"total":     nil,
		"unknown":   false,
		"error":     nil,
		"email":     emptyStringNil(EmailFromCred(cred)),
	}, nil
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

// apiGetP keeps the route explicit: account refresh/quota can use the proxy,
// while polling and asset retrieval remain direct.
func (c *Client) apiGetP(ctx context.Context, token, url string, useProxy bool) ([]byte, int, error) {
	client, err := c.newTLSClientP(useProxy)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req = req.WithContext(ctx)
	req.Header = http.Header{
		"accept":        {"application/json, text/plain, */*"},
		"authorization": {"Bearer " + token},
		"origin":        {webOrigin},
		"referer":       {webOrigin + "/"},
		"user-agent":    {userAgent},
		http.HeaderOrderKey: {
			"accept", "authorization", "origin", "referer", "user-agent",
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

func (c *Client) newSubmitTLSClient() (tlsclient.HttpClient, error) { return c.newProxyTLSClient() }

// newDirectTLSClient is used for project setup, polling, and downloads.
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

// ---------------------------------------------------------------------------
// Small util
// ---------------------------------------------------------------------------

func uuid4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
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
