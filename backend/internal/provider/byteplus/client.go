// Package byteplus implements the authenticated web protocol used by BytePlus
// Lumina. The durable credential is the browser Cookie header; authenticated
// writes additionally require the csrfToken cookie to be mirrored in
// X-Csrf-Token.
package byteplus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"backend/internal/provider/proxypool"
	"backend/internal/provider/proxysession"
)

const (
	defaultAPIBase = "https://lumi-api.console.byteplus.com/api"
	webOrigin      = "https://ai.byteplus.com"
	webReferer     = webOrigin + "/lumina/en"
	userAgent      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"

	ModelSeedream50Pro  = "seedream-5.0-pro"
	ModelGPTImage2      = "gpt-image-2"
	ModelSeedream50Lite = "seedream-5.0-lite"
	ModelNanoBanana2    = "nano-banana-2"
	ModelNanoBananaPro  = "nano-banana-pro"

	maxAPIResponseBytes = 8 << 20
)

// Variables, rather than constants, keep the protocol unit-testable without
// changing the production endpoints.
var apiBaseURL = defaultAPIBase

var (
	ErrAuth              = errors.New("byteplus auth failed")
	ErrQuotaExhausted    = errors.New("byteplus quota exhausted")
	ErrRiskControl       = errors.New("byteplus risk control")
	ErrInvalidParams     = errors.New("byteplus invalid parameters")
	ErrTemporaryUpstream = errors.New("byteplus upstream temporary error")
	// ErrTaskFailed means an already accepted provider task reached an explicit
	// terminal failure. It remains in the temporary provider class for account
	// health, but recovery callers must close the durable event instead of polling
	// the same failed parent forever.
	ErrTaskFailed = fmt.Errorf("%w: accepted task failed", ErrTemporaryUpstream)
	// ErrRetryableTaskFailed is the narrow exception to the usual account-level
	// no-resubmit policy. Lumina explicitly terminated the task because its beta
	// model was temporarily unstable, so 2API may advance through its separate,
	// bounded distinct-account policy after proving the attempt uncharged. It
	// still carries ErrTaskFailed and ErrTemporaryUpstream.
	ErrRetryableTaskFailed = fmt.Errorf("%w: explicit beta failure permits bounded account failover", ErrTaskFailed)
	// ErrTaskAccepted marks failures that happened after create_task returned a
	// parent task id. It remains a temporary-upstream error for API mapping, but
	// callers must not retry the whole generation because create_task is not
	// idempotent and a second submission can consume credits twice.
	ErrTaskAccepted = fmt.Errorf("%w: task already accepted; do not resubmit", ErrTemporaryUpstream)
	// ErrTaskSubmissionUnknown marks create_task calls whose response was lost,
	// temporary, or missing a task id. The request may still have been accepted,
	// so replaying it would violate at-most-once submission.
	ErrTaskSubmissionUnknown = fmt.Errorf("%w: task submission outcome unknown; do not resubmit", ErrTemporaryUpstream)
	// ErrUpstreamRejected marks responses in which Lumina itself refused the
	// request: an HTTP 4xx or a parsed envelope carrying a business error code.
	// Such a request was definitely not accepted, so it is safe to fail over
	// even when the business code is otherwise unrecognized and stays in the
	// temporary class. Lost, 5xx, and unparseable responses never carry it.
	ErrUpstreamRejected = errors.New("byteplus upstream rejected request")
	// ErrNoActivePlan is the code-200402 "No Active Combos." verdict: the account
	// holds no plan and therefore zero computing points. It is a quota outcome
	// with a definitive, probe-visible zero balance rather than a temporary blip.
	ErrNoActivePlan = fmt.Errorf("%w: account has no active plan", ErrQuotaExhausted)
)

// noActivePlanCode is Lumina's business code for an account without any combo.
const noActivePlanCode = 200402

// upstreamRejection keeps the classified cause's message and class while adding
// the ErrUpstreamRejected marker, so callers can distinguish "the server said no"
// from "the answer was lost" without changing user-visible error mapping.
type upstreamRejection struct{ cause error }

func (e *upstreamRejection) Error() string { return e.cause.Error() }

func (e *upstreamRejection) Unwrap() []error { return []error{ErrUpstreamRejected, e.cause} }

func rejectedByUpstream(cause error) error {
	if cause == nil {
		return nil
	}
	return &upstreamRejection{cause: cause}
}

// ModelSpec is the immutable upstream identity of one supported Lumina image
// model. Only these five records are exposed; the similarly named layer-
// decomposition service is intentionally excluded.
type ModelSpec struct {
	Key           string
	ID            string
	Name          string
	ReqKey        string
	MaxReferences int
}

var supportedModels = []ModelSpec{
	{Key: ModelSeedream50Pro, ID: "7657401949175693322", Name: "Seedream 5.0 Pro", ReqKey: "ByteDance-Seedream-5.0-pro", MaxReferences: 10},
	{Key: ModelGPTImage2, ID: "6824519374061285743", Name: "GPT Image 2 （Beta）", ReqKey: "gpt-image-2", MaxReferences: 14},
	{Key: ModelSeedream50Lite, ID: "7604761017696141358", Name: "Seedream 5.0 Lite", ReqKey: "ByteDance-Seedream-5.0", MaxReferences: 10},
	{Key: ModelNanoBanana2, ID: "8162745039814627354", Name: "Nano Banana 2 （Beta）", ReqKey: "gemini-3.1-fi", MaxReferences: 14},
	{Key: ModelNanoBananaPro, ID: "8162745039814627353", Name: "Nano Banana Pro（Beta）", ReqKey: "gemini_nbp", MaxReferences: 14},
}

// Models returns a copy of the five supported model records.
func Models() []ModelSpec {
	out := make([]ModelSpec, len(supportedModels))
	copy(out, supportedModels)
	return out
}

// LookupModel accepts the stable key, upstream inference id, req_key, display
// name, and a few punctuation-only aliases used by OpenAI-compatible callers.
func LookupModel(value string) (ModelSpec, bool) {
	want := normalizeModelName(value)
	// Public model ids are namespaced so they cannot collide with models from
	// other providers. Keep the upstream-facing aliases private to this closed
	// five-model catalog, but accept the public ids at the provider boundary.
	want = strings.TrimPrefix(want, "lumina-")
	for _, spec := range supportedModels {
		if want == normalizeModelName(spec.Key) || want == normalizeModelName(spec.ID) ||
			want == normalizeModelName(spec.ReqKey) || want == normalizeModelName(spec.Name) {
			return spec, true
		}
	}
	aliases := map[string]string{
		"seedream-5-pro":       ModelSeedream50Pro,
		"seedream-5-lite":      ModelSeedream50Lite,
		"gpt-image-2-beta":     ModelGPTImage2,
		"nano-banana-2-beta":   ModelNanoBanana2,
		"nano-banana-pro-beta": ModelNanoBananaPro,
		"gemini-3-1-fi":        ModelNanoBanana2,
		"gemini-nbp":           ModelNanoBananaPro,
	}
	if canonical := aliases[want]; canonical != "" {
		for _, spec := range supportedModels {
			if spec.Key == canonical {
				return spec, true
			}
		}
	}
	return ModelSpec{}, false
}

func normalizeModelName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	replacer := strings.NewReplacer("_", "-", " ", "-", "（", "-", "(", "-", "）", "", ")", "")
	value = replacer.Replace(value)
	for strings.Contains(value, "--") {
		value = strings.ReplaceAll(value, "--", "-")
	}
	return strings.Trim(value, "-")
}

type Client struct {
	proxyMu  sync.RWMutex
	proxy    string
	assigner proxypool.Assigner
}

func NewClient(proxy string) *Client {
	return &Client{proxy: strings.TrimSpace(proxy)}
}

// SetProxy updates the egress proxy used by subsequent calls.
func (c *Client) SetProxy(proxy string) {
	if c == nil {
		return
	}
	c.proxyMu.Lock()
	c.proxy = strings.TrimSpace(proxy)
	c.proxyMu.Unlock()
}

func (c *Client) proxyValue() string {
	if c == nil {
		return ""
	}
	c.proxyMu.RLock()
	defer c.proxyMu.RUnlock()
	return c.proxy
}

func (c *Client) SetAssigner(assigner proxypool.Assigner) {
	if c == nil {
		return
	}
	c.proxyMu.Lock()
	c.assigner = assigner
	c.proxyMu.Unlock()
}

func (c *Client) RotateProxySession(cookie string) {
	if c == nil {
		return
	}
	if assigner := c.assignerValue(); assigner != nil {
		assigner.Rotate(proxyAccountID(cookie))
	}
}

func (c *Client) assignerValue() proxypool.Assigner {
	if c == nil {
		return nil
	}
	c.proxyMu.RLock()
	defer c.proxyMu.RUnlock()
	return c.assigner
}

func proxyAccountID(material string) string {
	if key := proxysession.Key(material); key != "" {
		return key
	}
	return "shared"
}

func (c *Client) egressProxy(account string) string {
	if c == nil {
		return ""
	}
	if assigner := c.assignerValue(); assigner != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if url, ok := assigner.Assign(ctx, proxyAccountID(account), ""); ok {
			return url
		}
	}
	return c.proxyValue()
}

func (c *Client) newHTTPClient(timeout time.Duration, account string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Provider pools opt into a proxy explicitly. Do not silently inherit
	// HTTP(S)_PROXY from the host process when this client is configured direct.
	transport.Proxy = nil
	if rawProxy := c.egressProxy(account); rawProxy != "" {
		proxyURL, err := url.Parse(rawProxy)
		if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
			return nil, fmt.Errorf("%w: invalid proxy URL", ErrInvalidParams)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		// Authenticated Lumina requests carry a durable browser Cookie, a CSRF
		// token, and sometimes a replayable JSON body. Never let an upstream
		// redirect choose a new destination for those credentials. Callers that
		// intentionally support redirects (currently asset downloads only) must
		// replace this policy and validate every destination themselves.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

// IsBytePlusCookie conservatively identifies a Lumina credential. Anonymous
// BytePlus tracking cookies alone must not be mistaken for an account session.
func IsBytePlusCookie(value string) bool {
	value = normalizeCookie(value)
	if CSRFTokenFromCookie(value) == "" {
		return false
	}
	for _, name := range []string{"loginToken", "sessionid", "session_id", "byteplus_session", "passport_auth_status"} {
		if cookieValue(value, name) != "" {
			return true
		}
	}
	// Exported browser Cookie headers change names occasionally. A CSRF token
	// plus any non-tracking credential remains a better discriminator than a
	// hard-coded session-cookie name.
	for _, part := range strings.Split(value, ";") {
		name, cookieValue, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		lower := strings.ToLower(strings.TrimSpace(name))
		if strings.TrimSpace(cookieValue) != "" && lower != "csrftoken" && !strings.HasPrefix(lower, "__spti") && lower != "lang" && lower != "locale" {
			return true
		}
	}
	return false
}

// CSRFTokenFromCookie returns the decoded csrfToken cookie value without ever
// logging or otherwise exposing the rest of the credential.
func CSRFTokenFromCookie(cookie string) string {
	value := cookieValue(normalizeCookie(cookie), "csrfToken")
	if decoded, err := url.PathUnescape(value); err == nil {
		return decoded
	}
	return value
}

func normalizeCookie(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "cookie:") {
		value = strings.TrimSpace(value[len("cookie:"):])
	}
	if strings.HasPrefix(value, "{") {
		var wrapped map[string]any
		if json.Unmarshal([]byte(value), &wrapped) == nil {
			if inner, ok := wrapped["cookie"].(string); ok {
				value = strings.TrimSpace(inner)
			}
		}
	}
	return value
}

func cookieValue(cookie, name string) string {
	for _, part := range strings.Split(cookie, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.EqualFold(strings.TrimSpace(key), name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// FetchProfile validates the cookie against /user/current and returns the
// upstream account fields. A code-0 guest snapshot is still unauthenticated.
func (c *Client) FetchProfile(ctx context.Context, cookie string) (map[string]any, error) {
	cookie = normalizeCookie(cookie)
	if cookie == "" {
		return nil, ErrAuth
	}
	data, err := c.apiData(ctx, http.MethodGet, "/user/current", cookie, nil)
	if err != nil {
		return nil, err
	}
	profile, ok := data.(map[string]any)
	if !ok || isGuestProfile(profile) {
		return nil, ErrAuth
	}
	return profile, nil
}

func isGuestProfile(profile map[string]any) bool {
	role := strings.ToLower(strings.TrimSpace(stringValue(profile["role"])))
	userID := strings.TrimSpace(stringValue(profile["user_id"]))
	return len(profile) == 0 || role == "guest" || userID == "" || userID == "0" || (userID == "1" && role == "guest")
}

// FetchCreditsBalance reads the image credit balance. The newer account-
// resources endpoint is preferred; the older image-quota endpoint is retained
// as a compatibility fallback because account types expose different schemas.
func (c *Client) FetchCreditsBalance(ctx context.Context, cookie string) (map[string]any, error) {
	cookie = normalizeCookie(cookie)
	if cookie == "" {
		return nil, ErrAuth
	}
	data, err := c.apiData(ctx, http.MethodGet, "/user/get_user_resources?resource_id=lumi%2Fcomputing_points", cookie, nil)
	if err == nil {
		if balance, ok := quotaBalance(data); ok {
			return balance, nil
		}
		if hasNoActivePlan(data) {
			return zeroBalance("no active plan"), nil
		}
	} else if errors.Is(err, ErrAuth) {
		return nil, ErrAuth
	} else if errors.Is(err, ErrNoActivePlan) {
		return zeroBalance("no active plan"), nil
	}

	data, fallbackErr := c.apiData(ctx, http.MethodGet, "/inference/get_user_quota?type=image", cookie, nil)
	if fallbackErr != nil {
		if errors.Is(fallbackErr, ErrAuth) {
			return nil, ErrAuth
		}
		if errors.Is(fallbackErr, ErrNoActivePlan) {
			// "No Active Combos." is a definitive zero, not a failed probe. Leaving
			// it unknown would keep the account eligible for every request and let
			// it absorb submissions it can never serve.
			return zeroBalance("no active plan"), nil
		}
		reason := fallbackErr.Error()
		if err != nil {
			reason = err.Error() + "; " + reason
		}
		return unknownBalance(reason), nil
	}
	if balance, ok := quotaBalance(data); ok {
		return balance, nil
	}
	return unknownBalance("image quota missing from response"), nil
}

func quotaBalance(data any) (map[string]any, bool) {
	root, ok := data.(map[string]any)
	if !ok {
		return nil, false
	}
	if list, ok := root["quota_list"].([]any); ok && len(list) > 0 {
		var best map[string]any
		bestScore := -1
		for _, item := range list {
			candidate, ok := item.(map[string]any)
			if !ok {
				continue
			}
			identity := strings.ToLower(strings.Join([]string{
				stringValue(candidate["resource_id"]), stringValue(candidate["id"]),
				stringValue(candidate["type"]), stringValue(candidate["name"]),
			}, " "))
			score := 0
			if strings.Contains(identity, "lumi/computing_points") {
				score = 3
			} else if strings.Contains(identity, "image") || strings.Contains(identity, "pic") {
				score = 2
			} else if len(list) == 1 {
				score = 1
			}
			if score > bestScore {
				best, bestScore = candidate, score
			}
		}
		if best != nil {
			root = best
		}
	}

	remaining, hasRemaining := firstNumber(root, "remain_count", "remaining", "remain", "available", "balance")
	total, hasTotal := firstNumber(root, "quota", "total", "total_count", "limit")
	used, hasUsed := firstNumber(root, "used", "used_count", "consume", "consumed")
	if !hasRemaining && hasTotal && hasUsed {
		remaining, hasRemaining = total-used, true
	}
	if !hasUsed && hasTotal && hasRemaining {
		used, hasUsed = total-remaining, true
	}
	if !hasRemaining {
		return nil, false
	}
	if remaining < 0 {
		remaining = 0
	}
	out := map[string]any{
		"remaining": canonicalNumber(remaining),
		"used":      nil,
		"total":     nil,
		"unknown":   false,
		"error":     nil,
	}
	if hasUsed {
		out["used"] = canonicalNumber(used)
	}
	if hasTotal {
		out["total"] = canonicalNumber(total)
	}
	return out, true
}

func firstNumber(values map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		if value, exists := values[key]; exists {
			if parsed, ok := numberValue(value); ok {
				return parsed, true
			}
		}
	}
	return 0, false
}

func numberValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case float64:
		return typed, true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func canonicalNumber(value float64) any {
	if value == float64(int(value)) {
		return int(value)
	}
	return value
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

// zeroBalance is a known, schedulable-as-empty balance. It is not a failed
// probe: error stays nil so admin views do not report it as one.
func zeroBalance(reason string) map[string]any {
	return map[string]any{
		"remaining": 0,
		"used":      0,
		"total":     0,
		"unknown":   false,
		"error":     nil,
		"reason":    reason,
	}
}

// hasNoActivePlan recognizes the account-resources payload of an account that
// never claimed a plan: the combos list is present but empty and no quota entry
// exists. A payload without a combos field says nothing and stays unknown.
func hasNoActivePlan(data any) bool {
	root, ok := data.(map[string]any)
	if !ok {
		return false
	}
	combos, present := root["combos"]
	if !present {
		return false
	}
	switch typed := combos.(type) {
	case nil:
	case []any:
		if len(typed) > 0 {
			return false
		}
	default:
		return false
	}
	if list, ok := root["quota_list"].([]any); ok && len(list) > 0 {
		return false
	}
	return true
}

func (c *Client) apiData(ctx context.Context, method, path, cookie string, payload any) (any, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("%w: encode request: %v", ErrInvalidParams, err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(apiBaseURL, "/")+path, body)
	if err != nil {
		return nil, fmt.Errorf("%w: create request: %v", ErrTemporaryUpstream, err)
	}
	setAPIHeaders(req, cookie, payload != nil)
	client, err := c.newHTTPClient(90*time.Second, cookie)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTemporaryUpstream, err)
	}
	defer resp.Body.Close()
	raw, err := readLimited(resp.Body, maxAPIResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTemporaryUpstream, err)
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var envelope map[string]any
	decodeErr := decoder.Decode(&envelope)
	code, hasCode := responseCode(envelope)
	message := responseEnvelopeMessage(envelope)
	if message == "" {
		message = responseMessage(raw)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		classified := classifyUpstreamError(resp.StatusCode, code, message)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			// A 4xx is the gateway or application refusing the request outright.
			// Only 5xx and transport failures leave the outcome genuinely unknown.
			classified = rejectedByUpstream(classified)
		}
		return nil, classified
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("%w: non-json response", ErrTemporaryUpstream)
	}
	if hasCode && code != 0 && code != 200 {
		// A parsed business error inside an HTTP 200 envelope is Lumina's explicit
		// verdict on this request; nothing was created upstream.
		return nil, rejectedByUpstream(classifyUpstreamError(resp.StatusCode, code, message))
	}
	if data, exists := envelope["data"]; exists {
		return data, nil
	}
	return envelope, nil
}

func setAPIHeaders(req *http.Request, cookie string, jsonBody bool) {
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", webOrigin)
	req.Header.Set("Referer", webReferer)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Lang", "en")
	if cookie = normalizeCookie(cookie); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if csrf := CSRFTokenFromCookie(cookie); csrf != "" {
		req.Header.Set("X-Csrf-Token", csrf)
	}
	if jsonBody {
		req.Header.Set("Content-Type", "application/json")
	}
}

func responseCode(envelope map[string]any) (int, bool) {
	for _, key := range []string{"code", "error_code", "errorcode"} {
		if raw, ok := envelope[key]; ok {
			if code, parsed := intValue(raw); parsed {
				return code, true
			}
		}
	}
	if base, ok := envelope["base_response"].(map[string]any); ok {
		return responseCode(base)
	}
	return 0, false
}

func responseEnvelopeMessage(envelope map[string]any) string {
	if envelope == nil {
		return ""
	}
	for _, key := range []string{"message", "error_message", "error_msg", "msg", "detail", "error"} {
		value, exists := envelope[key]
		if !exists {
			continue
		}
		if nested, ok := value.(map[string]any); ok {
			if message := responseEnvelopeMessage(nested); message != "" {
				return message
			}
			continue
		}
		if message := strings.TrimSpace(stringValue(value)); message != "" {
			return message
		}
	}
	for _, key := range []string{"base_response", "data"} {
		if nested, ok := envelope[key].(map[string]any); ok {
			if message := responseEnvelopeMessage(nested); message != "" {
				return message
			}
		}
	}
	return ""
}

func classifyUpstreamError(httpStatus, code int, message string) error {
	message = strings.TrimSpace(message)
	lower := strings.ToLower(message)
	base := ErrTemporaryUpstream
	switch {
	case code == noActivePlanCode || strings.Contains(lower, "no active combo"):
		// The account never claimed a plan (observed together with
		// is_country_blocked=true). It has zero computing points, so this is a
		// quota verdict with a known-zero balance, not a transient failure.
		base = ErrNoActivePlan
	case code == 100000007:
		base = ErrQuotaExhausted
	case code == 100000008:
		base = ErrRiskControl
	case code == 1000000023:
		base = ErrInvalidParams
	case containsAny(lower, "quota", "credit", "insufficient balance", "insufficient points", "exceed limit", "points exhausted", "remain count"):
		base = ErrQuotaExhausted
	case containsAny(lower, "risk", "review disapproved", "moderation", "unsafe", "illegal content"):
		base = ErrRiskControl
	case containsAny(lower, "invalid param", "invalid argument", "not support"):
		base = ErrInvalidParams
	case explicitAuthFailure(httpStatus, code, lower):
		base = ErrAuth
	case httpStatus == http.StatusPaymentRequired:
		base = ErrQuotaExhausted
	case httpStatus == http.StatusBadRequest || httpStatus == http.StatusUnprocessableEntity:
		base = ErrInvalidParams
	}
	if message == "" {
		if code != 0 {
			message = fmt.Sprintf("upstream code %d", code)
		} else {
			message = fmt.Sprintf("upstream http %d", httpStatus)
		}
	}
	return fmt.Errorf("%w: %s", base, clipString(message, 240))
}

func explicitAuthFailure(httpStatus, code int, message string) bool {
	if containsAny(message,
		"logintoken", "login token", "not logged", "not login", "please log in", "please login",
		"unauthorized", "authentication", "credential", "invalid session", "session expired",
		"invalid csrf", "csrf token", "invalid cookie", "cookie expired", "token expired",
	) {
		return true
	}
	// HTTP 401 (and an equivalent envelope code) unambiguously describes a
	// missing or rejected authentication credential. A bare 403 does not: the
	// Lumina gateway also uses it for quota and policy decisions, so it remains
	// retryable unless the parsed response explicitly identifies authentication.
	return httpStatus == http.StatusUnauthorized || code == http.StatusUnauthorized
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func responseMessage(raw []byte) string {
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&payload) == nil {
		if message := responseEnvelopeMessage(payload); message != "" {
			return message
		}
	}
	return clipString(string(raw), 240)
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	limited := io.LimitReader(reader, limit+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("response exceeds size limit")
	}
	return raw, nil
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case nil:
		return ""
	default:
		encoded, _ := json.Marshal(typed)
		return string(encoded)
	}
}

func intValue(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		parsed, err := strconv.ParseFloat(typed.String(), 64)
		return int(parsed), err == nil
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return int(parsed), err == nil
	default:
		return 0, false
	}
}

func clipString(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		return value[:limit]
	}
	return value
}
