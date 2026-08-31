// Package runway implements the Runway (runwayml.com) provider client. For now
// it only covers account management — JWT detection, workspace/team id
// extraction and credit-balance probing — mirroring the curl_cffi reference in
// query_credits.py with tls-client so the JA3/JA4 fingerprint matches Chrome.
package runway

import (
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	http "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/google/uuid"
)

const (
	apiBase = "https://api.runwayml.com"
	origin  = "https://app.runwayml.com"

	// userAgent / secChUA mirror the real browser that produced a successful
	// registration + generation in the reference HAR (Edge 150 on Windows).
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0"
	secChUA   = `"Not;A=Brand";v="8", "Chromium";v="150", "Microsoft Edge";v="150"`

	// sourceApp / buildHash are the x-runway-source-application[-version] the real
	// web app stamps on EVERY authed API call (319/336 requests in the reference
	// HAR). buildHash is the web bundle's git sha; it also appears as the
	// `sentry-release` segment of the baggage header. Runway anti-abuse keys on
	// these being present + client-id (see clientIDFromToken): a write/upload that
	// lacks them is judged a non-web client and the account's free credits are
	// zeroed on the FIRST upload ("上传清零积分"). buildHash tracks Runway's web
	// releases — refresh it from a current HAR if uploads start getting flagged.
	sourceApp = "web"
	buildHash = "3e96b2f0f85b8c0cafb7c0dcd7a6878305aaa0f0"
)

// clientIDOverride, when non-empty, forces the x-runway-client-id (used to pin an
// account to the exact persistent device id its real browser session used).
var clientIDOverride string

// clientIDUUIDNamespace is a fixed namespace so clientIDFromToken is stable
// across processes/restarts — the same account always derives the same
// x-runway-client-id, exactly like the browser's localStorage-persisted id.
var clientIDUUIDNamespace = uuid.MustParse("6ba7b811-9dad-11d1-80b4-00c04fd430c8")

// clientIDFromToken derives the persistent per-account x-runway-client-id. The
// real SPA generates this UUID once and stores it in localStorage; every request
// from that browser reuses the SAME value (5b89c786-… appears on all 319 authed
// calls in the HAR). We reproduce that stability by deterministically deriving a
// v5 UUID from the account's JWT "id" claim, so one account == one client-id for
// its whole lifetime without needing a DB column. Falls back to the raw token if
// the id claim is missing.
func clientIDFromToken(token string) string {
	if clientIDOverride != "" {
		return clientIDOverride
	}
	seed := TeamIDFromToken(token)
	if seed == "" {
		seed = strings.TrimPrefix(token, "Bearer ")
	}
	return uuid.NewSHA1(clientIDUUIDNamespace, []byte("runway-client-id:"+seed)).String()
}

// randHex returns n random bytes hex-encoded (2n chars), for sentry trace ids.
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

// browserHeaders builds the full Chrome/Edge header set the Runway web app sends
// on its JSON API calls. tls-client only spoofs the TLS/JA3 fingerprint — it does
// NOT inject a User-Agent or the client-hint / fetch-metadata headers. Without
// them a request looks like a headless bot and Runway's anti-abuse revokes the
// account's free credits within minutes ("秒死"). teamID is optional.
func browserHeaders(token, teamID string) http.Header {
	traceID := randHex(16)
	spanID := randHex(8)
	h := http.Header{
		"accept":             {"application/json"},
		"accept-language":    {"zh-CN,zh;q=0.9,en;q=0.8,en-GB;q=0.7,en-US;q=0.6"},
		"authorization":      {"Bearer " + strings.TrimPrefix(token, "Bearer ")},
		"baggage":            {"sentry-environment=production,sentry-release=" + buildHash + ",sentry-public_key=8ea832c064ed4bbcb4b8952c02ba119a,sentry-trace_id=" + traceID},
		"content-type":       {"application/json"},
		"origin":             {origin},
		"priority":           {"u=1, i"},
		"referer":            {origin + "/"},
		"sec-ch-ua":          {secChUA},
		"sec-ch-ua-mobile":   {"?0"},
		"sec-ch-ua-platform": {`"Windows"`},
		"sec-fetch-dest":     {"empty"},
		"sec-fetch-mode":     {"cors"},
		"sec-fetch-site":     {"same-site"},
		"sentry-trace":       {traceID + "-" + spanID},
		"user-agent":         {userAgent},
		// x-runway-* identify this as the real web app to Runway's anti-abuse.
		// client-id must be persistent per account (see clientIDFromToken);
		// omitting these is what zeroes an account's credits on first upload.
		"x-runway-client-id":                  {clientIDFromToken(token)},
		"x-runway-source-application":         {sourceApp},
		"x-runway-source-application-version": {buildHash},
		"x-runway-workspace":                  {teamID},
		http.HeaderOrderKey: {
			"accept", "accept-language", "authorization", "baggage",
			"content-type", "origin", "priority", "referer",
			"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform",
			"sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site",
			"sentry-trace", "user-agent",
			"x-runway-client-id", "x-runway-source-application",
			"x-runway-source-application-version", "x-runway-workspace",
		},
	}
	if strings.TrimSpace(teamID) == "" {
		h.Del("x-runway-workspace")
	}
	return h
}

// browserAssetHeaders is the lighter header set the browser sends on cross-site
// asset transfers (presigned S3 upload / CloudFront artifact download): a
// User-Agent and client hints, but no Runway authorization.
func browserAssetHeaders() http.Header {
	return http.Header{
		"accept":             {"*/*"},
		"accept-language":    {"zh-CN,zh;q=0.9,en;q=0.8,en-GB;q=0.7,en-US;q=0.6"},
		"origin":             {origin},
		"referer":            {origin + "/"},
		"sec-ch-ua":          {secChUA},
		"sec-ch-ua-mobile":   {"?0"},
		"sec-ch-ua-platform": {`"Windows"`},
		"sec-fetch-dest":     {"empty"},
		"sec-fetch-mode":     {"cors"},
		"sec-fetch-site":     {"cross-site"},
		"user-agent":         {userAgent},
	}
}

var (
	ErrAuth              = errors.New("runway auth failed")
	ErrQuotaExhausted    = errors.New("runway quota exhausted")
	ErrTemporaryUpstream = errors.New("runway upstream temporary error")
)

type Client struct {
	proxyMu sync.RWMutex
	proxy   string
}

func NewClient(proxy string) *Client {
	return &Client{proxy: strings.TrimSpace(proxy)}
}

func (c *Client) proxyValue() string {
	c.proxyMu.RLock()
	defer c.proxyMu.RUnlock()
	return c.proxy
}

// IsRunwayToken reports whether a JWT looks like a Runway access token: a
// top-level numeric "id" plus an "sso" claim, and crucially NO OpenAI
// (https://api.openai.com/*) claims — that's what disambiguates it from a
// ChatGPT token, which is otherwise also an opaque three-part JWT.
func IsRunwayToken(token string) bool {
	claims := decodeJWTPayload(token)
	if len(claims) == 0 {
		return false
	}
	for k := range claims {
		if strings.HasPrefix(k, "https://api.openai.com/") {
			return false
		}
	}
	_, hasSSO := claims["sso"]
	return hasSSO && claims["id"] != nil
}

// TeamIDFromToken returns the Runway workspace/team id, which equals the JWT
// "id" claim (query_credits.py / gen_video.py both derive teamId this way).
func TeamIDFromToken(token string) string {
	claims := decodeJWTPayload(token)
	switch v := claims["id"].(type) {
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case json.Number:
		return v.String()
	case string:
		return strings.TrimSpace(v)
	default:
		return ""
	}
}

// FetchCreditsBalance probes the account's plan credits via /v1/profile/features
// (query_credits.py). Returns a normalized map mirroring the Adobe client so the
// TokenService quota plumbing can treat all providers uniformly. A 401/403 maps
// to ErrAuth (token dead); any other failure is reported as unknown without
// killing the account.
func (c *Client) FetchCreditsBalance(ctx context.Context, token string) (map[string]any, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return unknownBalance("empty token"), nil
	}
	teamID := TeamIDFromToken(token)
	if teamID == "" {
		return unknownBalance("no team id"), nil
	}

	client, err := c.newProxyTLSClient()
	if err != nil {
		return nil, err
	}
	url := apiBase + "/v1/profile/features?asTeamId=" + teamID
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header = browserHeaders(token, teamID)

	resp, err := client.Do(req)
	if err != nil {
		return unknownBalance("network: " + err.Error()), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 && runwayDefinitiveAuth(body) {
		return nil, ErrAuth
	}
	if resp.StatusCode == 403 {
		return unknownBalance("http 403"), nil
	}
	if resp.StatusCode != 200 {
		return unknownBalance(fmt.Sprintf("http %d: %s", resp.StatusCode, clip(body, 160))), nil
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return unknownBalance("non-json"), nil
	}
	features, _ := payload["features"].(map[string]any)
	permitted, _ := features["permitted"].(map[string]any)
	used, _ := features["used"].(map[string]any)
	total := intValue(permitted["numPlanCredits"])
	spent := intValue(used["numPlanCredits"])
	remaining := total - spent
	if remaining < 0 {
		remaining = 0
	}
	return map[string]any{
		"remaining": remaining,
		"used":      spent,
		"total":     total,
		"unknown":   false,
		"error":     nil,
	}, nil
}

func runwayDefinitiveAuth(body []byte) bool {
	text := strings.ToLower(string(body))
	for _, marker := range []string{
		"invalid_access_token", "invalid access token", "invalid_token",
		"token expired", "expired token", "jwt expired", "session expired",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
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

// newProxyTLSClient is used for authentication/account maintenance and submit.
func (c *Client) newProxyTLSClient() (tlsclient.HttpClient, error) { return c.newTLSClientP(true) }

func (c *Client) newSubmitTLSClient() (tlsclient.HttpClient, error) { return c.newProxyTLSClient() }

// newDirectTLSClient is used for project setup, media, polling, and downloads.
func (c *Client) newDirectTLSClient() (tlsclient.HttpClient, error) { return c.newTLSClientP(false) }

func (c *Client) newTLSClientP(useProxy bool) (tlsclient.HttpClient, error) {
	options := []tlsclient.HttpClientOption{
		// Whole-request timeout, incl. reading the response body. 30s was too
		// tight for downloading a rendered image/video artifact off a slow CDN,
		// surfacing as "request canceled (Client.Timeout ... while reading body)".
		// The caller's genCtx still bounds the overall generation.
		tlsclient.WithTimeoutSeconds(300),
		tlsclient.WithClientProfile(profiles.Chrome_133),
		tlsclient.WithRandomTLSExtensionOrder(),
	}
	if useProxy {
		if proxy := c.proxyValue(); proxy != "" {
			options = append(options, tlsclient.WithProxyUrl(proxy))
		}
	}
	return tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), options...)
}

func decodeJWTPayload(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(strings.TrimPrefix(token, "Bearer ")), ".")
	if len(parts) < 2 {
		return map[string]any{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	return out
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

func intValue(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	default:
		return 0
	}
}

func clip(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n]
	}
	return s
}
