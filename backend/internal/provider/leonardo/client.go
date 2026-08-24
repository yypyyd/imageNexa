// Package leonardo implements the Leonardo.ai (app.leonardo.ai) provider client.
// Unlike chatgpt/runway (whose JWT IS the stored credential), Leonardo's durable
// credential is the browser COOKIE (better-auth session): the bearer access token
// it mints lives only ~1h. So every call here takes the cookie and derives a
// fresh JWT on the fly via /api/auth/get-session — there is no long-lived token to
// store or a separate refresh profile to maintain. tls-client gives a Chrome
// JA3/JA4 fingerprint so the requests aren't flagged.
package leonardo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

const (
	appBase    = "https://app.leonardo.ai"
	graphqlURL = "https://api.leonardo.ai/v1/graphql"
	// schemaVersion must track the web app's x-leo-schema-version header: the
	// GraphQL gateway rejects a stale version with a generic
	// INTERNAL_SERVER_ERROR / "An error occurred." on the Generate mutation.
	schemaVersion = "1.258.8"
	userAgent     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"
)

var (
	ErrAuth              = errors.New("leonardo auth failed")
	ErrQuotaExhausted    = errors.New("leonardo quota exhausted")
	ErrTemporaryUpstream = errors.New("leonardo upstream temporary error")
)

type Client struct {
	proxyMu sync.RWMutex
	proxy   string
	// sessions caches the short-lived access token per cookie so we don't hit
	// /api/auth/get-session on every call — Leonardo rate-limits that endpoint
	// (429) hard, so re-using the ~1h JWT is essential.
	mu       sync.Mutex
	sessions map[string]*Session
}

func NewClient(proxy string) *Client {
	return &Client{proxy: strings.TrimSpace(proxy), sessions: map[string]*Session{}}
}

func (c *Client) proxyValue() string {
	c.proxyMu.RLock()
	defer c.proxyMu.RUnlock()
	return c.proxy
}

// IsLeonardoCookie reports whether a pasted credential is a Leonardo cookie: it
// carries the better-auth session cookie name. This is what disambiguates it from
// an Adobe cookie at import time.
func IsLeonardoCookie(value string) bool {
	return strings.Contains(value, "__Secure-better-auth.session_token") ||
		strings.Contains(value, "better-auth.session_data")
}

// Session is the result of /api/auth/get-session: the short-lived bearer plus the
// ids the GraphQL API needs (cognitoSub for the quota query, userId for the feed
// and the CDN image path) and the human-facing account fields.
type Session struct {
	AccessToken string
	CognitoSub  string
	UserID      string
	Email       string
	Name        string
	ExpiresAt   int64
}

// GetSession exchanges the cookie for a fresh access token + account ids. A 401/403
// (or a response with no access token) means the cookie/session is dead → ErrAuth.
func (c *Client) GetSession(ctx context.Context, cookie string) (*Session, error) {
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		return nil, ErrAuth
	}
	// Re-use a cached, still-valid access token (keep a 60s safety margin) instead
	// of hitting the heavily rate-limited get-session endpoint again.
	c.mu.Lock()
	if cs, ok := c.sessions[cookie]; ok && cs.ExpiresAt-60 > time.Now().Unix() {
		c.mu.Unlock()
		return cs, nil
	}
	c.mu.Unlock()

	// app.leonardo.ai sits behind Vercel's challenge edge, which answers the
	// residential proxy ranges with an HTML 429 checkpoint page while the plain
	// egress passes. Try direct first, fall back to the proxy.
	sess, err := c.fetchSession(ctx, cookie, false)
	if err != nil && !errors.Is(err, ErrAuth) && c.proxyValue() != "" {
		if viaProxy, perr := c.fetchSession(ctx, cookie, true); perr == nil {
			sess, err = viaProxy, nil
		}
	}
	if err != nil {
		return nil, err
	}
	if sess.ExpiresAt > time.Now().Unix() {
		c.mu.Lock()
		c.sessions[cookie] = sess
		c.mu.Unlock()
	}
	return sess, nil
}

// fetchSession performs the get-session exchange over one egress path.
func (c *Client) fetchSession(ctx context.Context, cookie string, useProxy bool) (*Session, error) {
	client, err := c.newTLSClientP(useProxy)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, appBase+"/api/auth/get-session", nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header = http.Header{
		"accept":          {"*/*"},
		"accept-language": {"en-US,en;q=0.9"},
		"cookie":          {cookie},
		"origin":          {appBase},
		"referer":         {appBase + "/"},
		"user-agent":      {userAgent},
		"sec-fetch-dest":  {"empty"},
		"sec-fetch-mode":  {"cors"},
		"sec-fetch-site":  {"same-origin"},
		http.HeaderOrderKey: {
			"accept", "accept-language", "cookie", "origin", "referer",
			"user-agent", "sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site",
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrTemporaryUpstream, err.Error())
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, ErrAuth
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%w: get-session http %d: %s", ErrTemporaryUpstream, resp.StatusCode, clip(body, 160))
	}
	var raw struct {
		Session struct {
			AccessToken  string `json:"accessToken"`
			CognitoSub   string `json:"cognitoSub"`
			UserID       string `json:"userId"`
			HasuraUserID string `json:"hasuraUserId"`
			TokenExpiry  int64  `json:"accessTokenExpiry"`
		} `json:"session"`
		User struct {
			ID    string `json:"id"`
			Email string `json:"email"`
			Name  string `json:"name"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("%w: get-session non-json", ErrTemporaryUpstream)
	}
	if strings.TrimSpace(raw.Session.AccessToken) == "" {
		// No bearer despite 200 → the cookie no longer authenticates.
		return nil, ErrAuth
	}
	uid := raw.Session.UserID
	if uid == "" {
		uid = raw.Session.HasuraUserID
	}
	if uid == "" {
		uid = raw.User.ID
	}
	return &Session{
		AccessToken: raw.Session.AccessToken,
		CognitoSub:  raw.Session.CognitoSub,
		UserID:      uid,
		Email:       strings.TrimSpace(raw.User.Email),
		Name:        strings.TrimSpace(raw.User.Name),
		ExpiresAt:   raw.Session.TokenExpiry,
	}, nil
}

const qGetTokens = `query GetUserTokensFromSub($sub: String) {
  user_details(where: {cognitoId: {_eq: $sub}}) {
    id
    plan
    subscriptionTokens
    paidTokens
    rolloverTokens
    tokenRenewalDate
    __typename
  }
}`

// FreeDailyTokens is the free tier's (普通号) daily allowance. A subscription
// balance above it can only come from a paid plan.
const FreeDailyTokens = 150

// paidPlanPrefixes are Leonardo's paid subscription tiers (annual variants carry a
// suffix, hence prefix matching). The free tier reports BASIC.
var paidPlanPrefixes = []string{"APPRENTICE", "ARTISAN", "MAESTRO"}

// IsPaidPlan reports whether an account is a 积分号 —— 余额是买来的点数 / 订阅点数,
// 按 tokenRenewalDate 月度续期而不是每日重置。只在有明确证据时才判为积分号(购买/
// 结转点数、已知付费 plan、或订阅点数超过免费号每日上限);其余一律当普通号,
// 保持原有每日重置逻辑不变。
func IsPaidPlan(plan string, subscriptionTokens, paidTokens, rolloverTokens int) bool {
	if paidTokens > 0 || rolloverTokens > 0 || subscriptionTokens > FreeDailyTokens {
		return true
	}
	p := strings.ToUpper(strings.TrimSpace(plan))
	for _, prefix := range paidPlanPrefixes {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// FetchCreditsBalance derives a JWT from the cookie then reads the account's image
// token balance. Returns a normalized map mirroring the other providers so the
// TokenService quota plumbing is uniform. remaining = subscription+paid+rollover
// (the spendable image tokens); available_until carries the renewal time so
// the maintenance sweep can auto-recover a 限额 account. paid marks a 积分号 so the
// callers don't apply the free tier's daily-reset handling to it.
func (c *Client) FetchCreditsBalance(ctx context.Context, cookie string) (map[string]any, error) {
	sess, err := c.GetSession(ctx, cookie)
	if err != nil {
		if errors.Is(err, ErrAuth) {
			return nil, ErrAuth
		}
		return unknownBalance(err.Error()), nil
	}
	if sess.CognitoSub == "" {
		return unknownBalance("no cognitoSub"), nil
	}

	payload, _ := json.Marshal(map[string]any{
		"operationName": "GetUserTokensFromSub",
		"variables":     map[string]any{"sub": sess.CognitoSub},
		"query":         qGetTokens,
	})
	body, status, err := c.graphqlP(ctx, sess.AccessToken, payload, true)
	if err != nil {
		return unknownBalance("network: " + err.Error()), nil
	}
	if status == 401 || status == 403 {
		return nil, ErrAuth
	}
	if status != 200 {
		return unknownBalance(fmt.Sprintf("http %d: %s", status, clip(body, 160))), nil
	}
	var result struct {
		Data struct {
			UserDetails []struct {
				Plan               string `json:"plan"`
				SubscriptionTokens int    `json:"subscriptionTokens"`
				PaidTokens         int    `json:"paidTokens"`
				RolloverTokens     int    `json:"rolloverTokens"`
				TokenRenewalDate   string `json:"tokenRenewalDate"`
			} `json:"user_details"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return unknownBalance("non-json"), nil
	}
	if len(result.Data.UserDetails) == 0 {
		return unknownBalance("no user_details"), nil
	}
	ud := result.Data.UserDetails[0]
	remaining := ud.SubscriptionTokens + ud.PaidTokens + ud.RolloverTokens
	return map[string]any{
		"remaining":       remaining,
		"used":            nil,
		"total":           nil,
		"unknown":         false,
		"error":           nil,
		"plan":            ud.Plan,
		"paid":            IsPaidPlan(ud.Plan, ud.SubscriptionTokens, ud.PaidTokens, ud.RolloverTokens),
		"available_until": strings.TrimSpace(ud.TokenRenewalDate),
		"email":           emptyStringNil(sess.Email),
		"display_name":    emptyStringNil(sess.Name),
		"user_id":         emptyStringNil(sess.UserID),
	}, nil
}

// submitGraphQL is reserved for a generation-submit mutation. graphqlP lets
// callers explicitly choose proxy or direct egress for each route.
func (c *Client) submitGraphQL(ctx context.Context, accessToken string, payload []byte) ([]byte, int, error) {
	return c.graphqlP(ctx, accessToken, payload, true)
}

func (c *Client) graphqlP(ctx context.Context, accessToken string, payload []byte, useProxy bool) ([]byte, int, error) {
	client, err := c.newTLSClientP(useProxy)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest(http.MethodPost, graphqlURL, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	req = req.WithContext(ctx)
	req.Header = http.Header{
		"content-type":         {"application/json"},
		"accept":               {"*/*"},
		"accept-language":      {"en-US,en;q=0.9"},
		"origin":               {appBase},
		"referer":              {appBase + "/"},
		"user-agent":           {userAgent},
		"authorization":        {"Bearer " + accessToken},
		"x-leo-schema-version": {schemaVersion},
		"sec-fetch-dest":       {"empty"},
		"sec-fetch-mode":       {"cors"},
		"sec-fetch-site":       {"same-site"},
		http.HeaderOrderKey: {
			"content-type", "accept", "accept-language", "origin", "referer",
			"user-agent", "authorization", "x-leo-schema-version",
			"sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site",
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func unknownBalance(reason string) map[string]any {
	return map[string]any{
		"remaining": nil,
		"used":      nil,
		"total":     nil,
		"unknown":   true,
		"error":     reason,
	}
}

// newDirectTLSClient is used for project/media setup, polling, and downloads.
func (c *Client) newDirectTLSClient() (tlsclient.HttpClient, error) { return c.newTLSClientP(false) }

func (c *Client) newTLSClientP(useProxy bool) (tlsclient.HttpClient, error) {
	// Match the fingerprint proven to work against Leonardo's Cloudflare edge:
	// Chrome_120, fixed extension order. A randomized JA3 (Chrome_133 +
	// WithRandomTLSExtensionOrder) gets flagged and 429'd at get-session.
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

// downloadImage fetches a generated image (cdn.leonardo.ai) and returns the bytes.
func (c *Client) downloadImage(ctx context.Context, imageURL string) ([]byte, error) {
	if _, err := url.Parse(imageURL); err != nil {
		return nil, err
	}
	client, err := c.newDirectTLSClient()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, imageURL, nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header = http.Header{
		"accept":     {"image/avif,image/webp,image/png,image/*,*/*;q=0.8"},
		"user-agent": {userAgent},
		"referer":    {appBase + "/"},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%w: image download http %d", ErrTemporaryUpstream, resp.StatusCode)
	}
	return body, nil
}

func emptyStringNil(v string) any {
	v = strings.TrimSpace(v)
	if v == "" {
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
