package byteplus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

var (
	passportLoginBaseURL = "https://console.byteplus.com/api/passport"
	luminaLoginAPIBase   = "https://lumi-api.console.byteplus.com/api"
)

type passportLoginResponse struct {
	ResponseMetadata struct {
		Error *struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		} `json:"Error"`
	} `json:"ResponseMetadata"`
}

// Login exchanges a Lumina email/password for a fresh 48-hour browser session.
// It is intentionally a narrow login flow: registration, mailbox access and
// account creation do not belong in 2API.
func (c *Client) Login(ctx context.Context, identity, secret string) (string, error) {
	identity, secret = strings.TrimSpace(identity), strings.TrimSpace(secret)
	if identity == "" || secret == "" {
		return "", fmt.Errorf("byteplus login identity and secret are required")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		cookie, err := c.loginOnce(ctx, identity, secret)
		if err == nil {
			return cookie, nil
		}
		lastErr = err
		if attempt < 3 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}
	return "", lastErr
}

func (c *Client) loginOnce(ctx context.Context, identity, secret string) (string, error) {
	opts := []tlsclient.HttpClientOption{
		tlsclient.WithTimeoutSeconds(60),
		tlsclient.WithClientProfile(profiles.Chrome_131),
		tlsclient.WithCookieJar(tlsclient.NewCookieJar()),
	}
	if proxy := strings.TrimSpace(c.egressProxy(identity)); proxy != "" {
		opts = append(opts, tlsclient.WithProxyUrl(proxy))
	}
	client, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), opts...)
	if err != nil {
		return "", fmt.Errorf("create byteplus login client: %w", err)
	}
	client.SetFollowRedirect(true)

	login := &bytePlusLoginClient{client: client}
	if err := login.passport(ctx, "/login/getLoginCredential", map[string]any{}); err != nil {
		return "", fmt.Errorf("initialize byteplus login: %w", err)
	}
	if err := login.passport(ctx, "/login/mixtureLogin", map[string]any{
		"Identity": identity, "Password": secret, "EventName": "AuthAccountWithPassword",
	}); err != nil {
		return "", fmt.Errorf("byteplus password login: %w", err)
	}
	if login.cookie("digest") == "" || login.cookie("AccountID") == "" {
		return "", fmt.Errorf("byteplus login did not return a complete session")
	}
	login.syncLuminaCookies()
	// Hydrate the Lumina origin before exporting. These calls are best-effort;
	// the durable login verdict is the digest/AccountID pair above.
	_ = login.request(ctx, http.MethodPost, luminaLoginAPIBase+"/user/flag", map[string]any{
		"has_agreed_terms_and_legal_age": true,
		"lumi_seedance2_my_portrait":     true,
	})
	_ = login.request(ctx, http.MethodGet, luminaLoginAPIBase+"/user/current", nil)
	_ = login.request(ctx, http.MethodGet, luminaLoginAPIBase+"/user/get_user_resources", nil)

	cookie := login.cookieHeader()
	if !IsBytePlusCookie(cookie) {
		return "", fmt.Errorf("byteplus login returned an incomplete Cookie")
	}
	return cookie, nil
}

type bytePlusLoginClient struct {
	client tlsclient.HttpClient
}

func (c *bytePlusLoginClient) passport(ctx context.Context, path string, payload any) error {
	raw, status, err := c.do(ctx, http.MethodPost, passportLoginBaseURL+path, payload)
	if err != nil {
		return err
	}
	var parsed passportLoginResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("passport returned non-JSON status %d", status)
	}
	if parsed.ResponseMetadata.Error != nil {
		return fmt.Errorf("%s: %s", parsed.ResponseMetadata.Error.Code, parsed.ResponseMetadata.Error.Message)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("passport returned HTTP %d", status)
	}
	return nil
}

func (c *bytePlusLoginClient) request(ctx context.Context, method, rawURL string, payload any) error {
	_, status, err := c.do(ctx, method, rawURL, payload)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("HTTP %d", status)
	}
	return nil
}

func (c *bytePlusLoginClient) do(ctx context.Context, method, rawURL string, payload any) ([]byte, int, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, 0, err
	}
	req.Header = http.Header{
		"accept":             {"application/json, text/plain, */*"},
		"accept-language":    {"en-US,en;q=0.9"},
		"content-type":       {"application/json"},
		"origin":             {"https://ai.byteplus.com"},
		"referer":            {"https://ai.byteplus.com/"},
		"sec-ch-ua":          {`"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`},
		"sec-ch-ua-mobile":   {"?0"},
		"sec-ch-ua-platform": {`"Windows"`},
		"sec-fetch-dest":     {"empty"},
		"sec-fetch-mode":     {"cors"},
		"sec-fetch-site":     {"cross-site"},
		"user-agent":         {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"},
	}
	if csrf := c.cookie("csrfToken"); csrf != "" {
		req.Header.Set("x-csrf-token", csrf)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return raw, resp.StatusCode, err
}

func (c *bytePlusLoginClient) cookie(name string) string {
	u, _ := url.Parse("https://console.byteplus.com")
	for _, cookie := range c.client.GetCookies(u) {
		if cookie.Name == name {
			return cookie.Value
		}
	}
	return ""
}

func (c *bytePlusLoginClient) syncLuminaCookies() {
	source, _ := url.Parse("https://console.byteplus.com")
	target, _ := url.Parse("https://lumi-api.console.byteplus.com")
	cookies := c.client.GetCookies(source)
	copied := make([]*http.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		copied = append(copied, &http.Cookie{Name: cookie.Name, Value: cookie.Value, Path: "/"})
	}
	c.client.SetCookies(target, copied)
}

func (c *bytePlusLoginClient) cookieHeader() string {
	seen := map[string]string{}
	for _, base := range []string{"https://console.byteplus.com", "https://lumi-api.console.byteplus.com"} {
		u, _ := url.Parse(base)
		for _, cookie := range c.client.GetCookies(u) {
			if cookie.Name != "" && cookie.Value != "" {
				if _, exists := seen[cookie.Name]; !exists {
					seen[cookie.Name] = cookie.Value
				}
			}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+seen[name])
	}
	return strings.Join(parts, "; ")
}
