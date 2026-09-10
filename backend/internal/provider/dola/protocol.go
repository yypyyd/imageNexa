package dola

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// No browser, captured request, or another account's storage is used. Passport
// authenticates the imported cookie; Alice and TEA allocate separate identities.
func (c *Client) prepareProtocolAccount(ctx context.Context, account Account) (Account, error) {
	account = account.normalized()
	if strings.TrimRight(c.baseURL, "/") == apiBase {
		proxy, err := c.AccountProxy(ctx, account)
		if err != nil {
			return account, err
		}
		account.ProtocolProxy = proxy
	}
	query := commonQueryForTab(account, "")
	query.Set("account_sdk_source", "web")
	query.Set("sdk_version", "2.2.11-doubao.0")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/passport/account/info/v2/")+"?"+query.Encode(), nil)
	if err != nil {
		return account, err
	}
	setHeaders(req, account, "application/json", "str", c.endpoint("/chat/"))
	client, err := c.accountClient(ctx, account)
	if err != nil {
		return account, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return account, fmt.Errorf("%w: protocol login request failed", ErrTemporaryUpstream)
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	resp.Body.Close()
	if readErr != nil {
		return account, fmt.Errorf("%w: protocol login response incomplete", ErrTemporaryUpstream)
	}
	var passport struct {
		Message string `json:"message"`
		Data    struct {
			UserID    string `json:"user_id_str"`
			Visitor   bool   `json:"is_visitor_account"`
			ErrorCode int    `json:"error_code"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &passport) != nil {
		return account, fmt.Errorf("%w: protocol login response invalid", ErrTemporaryUpstream)
	}
	if resp.StatusCode == 401 || passport.Data.ErrorCode != 0 || passport.Data.Visitor {
		return account, ErrAuth
	}
	if resp.StatusCode != 200 || passport.Message != "success" || passport.Data.UserID == "" {
		return account, ErrAuth
	}
	account.Cookie = mergeProtocolCookies(account.Cookie, resp.Cookies())
	account, err = c.ensureProtocolDeviceCookie(ctx, account)
	if err != nil {
		return account, err
	}
	query = commonQueryForTab(account, "")
	query.Del("channel")
	query.Set("language", "en")
	query.Set("pc_version", "3.36.0")
	query.Set("doubao_pc_version", "3.36.0")
	query.Set("doubao_device_platform", "web")
	query.Set("real_aid", dolaAID)
	query.Set("pkg_type", "release_version")
	query.Set("tz_name", "UTC")
	data, status, err := c.signedProtocolJSON(ctx, account, "/alice/user/get_web_anon_id", query, map[string]any{}, false)
	if err != nil {
		return account, err
	}
	var identity struct {
		Code  int             `json:"code"`
		UID   json.RawMessage `json:"uid"`
		WebID json.RawMessage `json:"web_id"`
	}
	if status != 200 || json.Unmarshal(data, &identity) != nil || identity.Code != 0 {
		return account, fmt.Errorf("%w: protocol device initialization failed", ErrTemporaryUpstream)
	}
	webID := strings.Trim(string(identity.WebID), "\"")
	if webID == "" || webID == "0" || webID == "null" {
		return account, fmt.Errorf("%w: protocol device identity missing", ErrTemporaryUpstream)
	}
	// Alice/TTWid's web_id is the device_id. The website obtains query web_id
	// and tea_uuid from the separate TEA SDK token registration, not Passport.
	teaID, err := c.protocolWebID(ctx, account)
	if err != nil {
		return account, err
	}
	query.Set("web_id", teaID)
	query.Set("device_id", webID)
	query.Set("tea_uuid", teaID)
	account.ProtocolQuery = query
	return account, nil
}

type protocolIdentity struct {
	mu    sync.Mutex
	webID string
	ttwid string
}

// Login exports may omit device cookies. Fetch only the HTML response once
// per session to receive the server-issued ttwid; never load browser assets.
func (c *Client) ensureProtocolDeviceCookie(ctx context.Context, account Account) (Account, error) {
	if CookieValue(account.Cookie, "ttwid") != "" {
		return account, nil
	}
	value, _ := c.protocolIdentities.LoadOrStore(IdentityHash(account.SessionID), &protocolIdentity{})
	identity := value.(*protocolIdentity)
	identity.mu.Lock()
	defer identity.mu.Unlock()
	if identity.ttwid == "" {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/chat/"), nil)
		if err != nil {
			return account, err
		}
		setHeaders(req, account, "text/html", "str", c.endpoint("/chat/"))
		client, err := c.accountClient(ctx, account)
		if err != nil {
			return account, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return account, fmt.Errorf("%w: protocol device cookie request failed", ErrTemporaryUpstream)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2<<20))
		if resp.StatusCode != http.StatusOK {
			return account, fmt.Errorf("%w: protocol device cookie response failed", ErrTemporaryUpstream)
		}
		for _, cookie := range resp.Cookies() {
			if cookie.Name == "ttwid" && cookie.Value != "" && cookie.MaxAge >= 0 {
				identity.ttwid = cookie.Value
			}
		}
		if identity.ttwid == "" {
			return account, fmt.Errorf("%w: protocol device cookie missing", ErrTemporaryUpstream)
		}
	}
	account.Cookie = mergeProtocolCookies(account.Cookie, []*http.Cookie{{Name: "ttwid", Value: identity.ttwid}})
	return account, nil
}

func (c *Client) protocolWebID(ctx context.Context, account Account) (string, error) {
	account = account.normalized()
	if account.SessionID == "" {
		return "", ErrAuth
	}
	value, _ := c.protocolIdentities.LoadOrStore(IdentityHash(account.SessionID), &protocolIdentity{})
	identity := value.(*protocolIdentity)
	identity.mu.Lock()
	defer identity.mu.Unlock()
	if identity.webID != "" {
		return identity.webID, nil
	}
	client, err := c.accountClient(ctx, account)
	if err != nil {
		return "", err
	}
	id, err := registerProtocolWebID(ctx, client, "https://mcs-sg.ciciai.com/webid", account.UserAgent)
	if err == nil {
		identity.webID = id
	}
	return id, err
}

// Match TEA 5.2.3_oversea remoteWebid. This cross-origin request must not
// contain the Dola session cookie, fingerprint, or signed chat parameters.
func registerProtocolWebID(ctx context.Context, client *http.Client, endpoint, userAgent string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	payload, _ := json.Marshal(map[string]any{
		"app_id": 495671, "url": apiBase + "/chat/", "user_agent": userAgent,
		"referer": "", "user_unique_id": "",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Origin", apiBase)
	req.Header.Set("Referer", apiBase+"/")
	// Keep this registration credential-free even if the shared HTTP client
	// gains a cookie jar later. Redirects are not part of the SDK endpoint.
	registrationClient := *client
	registrationClient.Jar = nil
	registrationClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := registrationClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: protocol analytics identity request failed", ErrTemporaryUpstream)
	}
	defer resp.Body.Close()
	var result struct {
		Code  *int            `json:"e"`
		WebID json.RawMessage `json:"web_id"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result) != nil || result.Code == nil || *result.Code != 0 {
		return "", fmt.Errorf("%w: protocol analytics identity response invalid", ErrTemporaryUpstream)
	}
	id := strings.Trim(string(result.WebID), "\"")
	if len(id) < 1 || len(id) > 32 || strings.Trim(id, "0123456789") != "" || strings.Trim(id, "0") == "" {
		return "", fmt.Errorf("%w: protocol analytics identity missing", ErrTemporaryUpstream)
	}
	return id, nil
}

func mergeProtocolCookies(existing string, cookies []*http.Cookie) string {
	values := map[string]string{}
	order := []string{}
	for _, part := range strings.Split(existing, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok {
			if _, found := values[k]; !found {
				order = append(order, k)
			}
			values[k] = v
		}
	}
	for _, cookie := range cookies {
		if !dolaCookieAllowed(cookie.Name) {
			continue
		}
		if _, found := values[cookie.Name]; !found {
			order = append(order, cookie.Name)
		}
		if cookie.MaxAge < 0 {
			delete(values, cookie.Name)
		} else {
			values[cookie.Name] = cookie.Value
		}
	}
	parts := []string{}
	for _, k := range order {
		if v, ok := values[k]; ok {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, "; ")
}

func signProtocolURL(ctx context.Context, account Account, destination string, payload []byte) (string, error) {
	root := strings.TrimSpace(os.Getenv("DOLA_PROTOCOL_DIR"))
	if root == "" {
		root = "/app/scripts/dola-protocol"
	}
	platform := "Win32"
	if strings.Contains(account.UserAgent, "Linux") {
		platform = "Linux x86_64"
	} else if strings.Contains(account.UserAgent, "Macintosh") {
		platform = "MacIntel"
	}
	input := map[string]any{"url": destination, "body": string(payload), "cookie": account.Cookie, "userAgent": account.UserAgent, "pageURL": apiBase + "/chat/", "platform": platform}
	raw, _ := json.Marshal(input)
	signing, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(signing, "node", filepath.Join(root, "sign_request.cjs"))
	cmd.Stdin = bytes.NewReader(raw)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%w: offline request signing failed", ErrTemporaryUpstream)
	}
	var signed struct {
		URL  string `json:"url"`
		Body string `json:"body"`
	}
	if json.Unmarshal(out, &signed) != nil {
		return "", fmt.Errorf("%w: invalid signer output", ErrTemporaryUpstream)
	}
	original, _ := url.Parse(destination)
	parsed, err := url.Parse(signed.URL)
	if err != nil || parsed.Scheme != original.Scheme || parsed.Host != original.Host || parsed.Path != original.Path || parsed.User != nil || parsed.Query().Get("a_bogus") == "" || signed.Body != string(payload) {
		return "", errors.New("dola: signer changed request identity")
	}
	return signed.URL, nil
}

func (c *Client) signedProtocolJSON(ctx context.Context, account Account, path string, query url.Values, body any, submission bool) ([]byte, int, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	destination, err := signProtocolURL(ctx, account, c.endpoint(path)+"?"+query.Encode(), payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, destination, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	setHeaders(req, account, "application/json", "str", c.endpoint("/chat/"))
	if submission {
		req.Header.Set("Agw-Js-Conv", "str, str")
		req.Header.Set("last-event-id", "undefined")
	}
	client, err := c.accountClient(ctx, account)
	if err != nil {
		return nil, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		if submission {
			return nil, 0, ErrTaskSubmissionUnknown
		}
		return nil, 0, ErrTemporaryUpstream
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSSEBytes))
	if err != nil && submission {
		// Preserve any acknowledged conversation even if the stream breaks later.
		events, _ := parseSSE(data)
		for _, event := range events {
			if event.name == "SSE_ACK" {
				return data, resp.StatusCode, nil
			}
		}
		return nil, resp.StatusCode, ErrTaskSubmissionUnknown
	}
	return data, resp.StatusCode, err
}
func (c *Client) protocolCompletion(ctx context.Context, account Account, query url.Values, body any) ([]byte, int, error) {
	return c.signedProtocolJSON(ctx, account, completionPath, query, body, true)
}

// Reproduce the HTTP bootstrap used by the verified 2.5 / 30-second run.
// These configure the video entry without submitting a generation. Do not
// consume the default duration/model from their unpatched menu responses.
func (c *Client) initializeProtocolVideo(ctx context.Context, account Account, query url.Values) error {
	for _, step := range []struct {
		path string
		body any
	}{
		{"/alice/user/launch/core", map[string]any{}},
		{"/samantha/skill/pack", map[string]any{"skill_type": 17}},
		{"/alice/slot/action_bar_v3/brief_list", map[string]any{"language_code": "en", "bot_id": dolaBotID}},
		{"/alice/slot/action_bar_v3/get_item_conf", map[string]any{
			"language_code": "en", "bot_id": dolaBotID,
			"item_ids": []string{"1064442771335697", "1064442771335185", "1064442771335441", "1064442771335953", "1064442771336209"},
		}},
	} {
		data, status, err := c.signedProtocolJSON(ctx, account, step.path, query, step.body, false)
		if err != nil {
			return err
		}
		if err := validateProtocolInitialization(data, status); err != nil {
			return fmt.Errorf("%w: %s", err, step.path)
		}
	}
	return nil
}

func validateProtocolInitialization(data []byte, status int) error {
	var result struct {
		Code *int `json:"code"`
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return ErrAuth
	}
	if status != http.StatusOK || json.Unmarshal(data, &result) != nil || result.Code == nil {
		return fmt.Errorf("%w: invalid video initialization response", ErrTemporaryUpstream)
	}
	if isAuthCode(*result.Code) {
		return ErrAuth
	}
	if *result.Code != 0 {
		return fmt.Errorf("%w: video initialization rejected", ErrTemporaryUpstream)
	}
	return nil
}

// VerifyProtocolSession checks authentication, device allocation and signing,
// without sending chat completions or consuming a video generation.
func (c *Client) VerifyProtocolSession(ctx context.Context, account Account) error {
	_, err := c.prepareProtocolAccount(ctx, account)
	return err
}
