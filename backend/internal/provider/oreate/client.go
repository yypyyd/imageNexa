// Package oreate implements the OreateAI website provider. OreateAI does not
// expose an OpenAI-compatible API: requests use a website cookie, create an AI
// video chat, attach a Banti risk token, and consume a server-sent event stream.
package oreate

import (
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
)

const (
	apiBase      = "https://www.oreateai.com"
	defaultUA    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
	defaultRefer = apiBase + "/home/chat/aiVideo"
)

var (
	ErrAuth              = errors.New("oreate auth failed")
	ErrQuotaExhausted    = errors.New("oreate quota exhausted")
	ErrRiskControl       = errors.New("oreate risk control")
	ErrSpamUser          = errors.New("oreate spam user")
	ErrTemporaryUpstream = errors.New("oreate upstream temporary error")
	ErrContentRejected   = errors.New("oreate content rejected")
)

type Account struct {
	Cookie    string
	UserAgent string
	Email     string
	OUID      string
	BID       string
	VIP       string
	RegTS     int64
}

func (a Account) normalized() Account {
	a.Cookie = strings.TrimSpace(a.Cookie)
	a.UserAgent = strings.TrimSpace(a.UserAgent)
	if a.UserAgent == "" {
		a.UserAgent = defaultUA
	}
	if a.OUID == "" {
		a.OUID = cookieValue(a.Cookie, "OUID")
	}
	if a.BID == "" {
		a.BID = cookieValue(a.Cookie, "__bid_n")
	}
	if a.VIP == "" {
		a.VIP = "0"
	}
	return a
}

type Signature struct {
	JT     string
	BID    string
	Cookie string
}

type Signer interface {
	Sign(context.Context, Account) (Signature, error)
}

type Client struct {
	proxyMu      sync.RWMutex
	proxy        string
	signer       Signer
	baseURL      string
	cdnBaseURL   string
	directClient *http.Client
}

func NewClient(proxy string) *Client {
	c := &Client{proxy: strings.TrimSpace(proxy), baseURL: apiBase}
	c.signer = newChromiumSigner(c.proxyValue)
	return c
}

func (c *Client) endpoint(path string) string {
	base := strings.TrimRight(strings.TrimSpace(c.baseURL), "/")
	if base == "" {
		base = apiBase
	}
	return base + path
}

func (c *Client) SetProxy(proxy string) {
	c.proxyMu.Lock()
	c.proxy = strings.TrimSpace(proxy)
	c.proxyMu.Unlock()
}

func (c *Client) proxyValue() string {
	c.proxyMu.RLock()
	defer c.proxyMu.RUnlock()
	return c.proxy
}

func (c *Client) httpClient(useProxy bool) *http.Client {
	if !useProxy {
		if c.directClient != nil {
			return c.directClient
		}
		return c.httpClientVia("")
	}
	return c.httpClientVia(c.proxyValue())
}

// httpClientVia sends through one specific proxy URL, which the video path uses
// to keep every request of a generation on the exit IP that minted its token.
func (c *Client) httpClientVia(proxyURL string) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if raw := strings.TrimSpace(proxyURL); raw != "" {
		if parsed, err := url.Parse(raw); err == nil {
			transport.Proxy = http.ProxyURL(parsed)
		}
	}
	return &http.Client{Transport: transport}
}

// egressProxy reports the proxy a request has to leave through: the session
// pinned to a token when the caller has one, otherwise the configured proxy.
func (c *Client) egressProxy(proxyURL string) string {
	if raw := strings.TrimSpace(proxyURL); raw != "" {
		return raw
	}
	return c.proxyValue()
}

func (c *Client) egressClient(proxyURL string) *http.Client {
	return c.httpClientVia(c.egressProxy(proxyURL))
}

// signHot mints a token together with the proxy session its requests must use.
// Signers without a warm pool (tests) fall back to the plain signature and the
// configured proxy.
func (c *Client) signHot(ctx context.Context, account Account) (hotSignature, error) {
	if hot, ok := c.signer.(hotSigner); ok {
		return hot.SignHot(ctx, account)
	}
	sig, err := c.signer.Sign(ctx, account)
	if err != nil {
		return hotSignature{}, err
	}
	return hotSignature{Signature: sig, Proxy: c.proxyValue()}, nil
}

func setHeaders(req *http.Request, account Account, accept string) {
	account = account.normalized()
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", account.Cookie)
	req.Header.Set("User-Agent", account.UserAgent)
	req.Header.Set("Client-Type", "pc")
	req.Header.Set("locale", "zh-CN")
	req.Header.Set("Origin", apiBase)
	req.Header.Set("Referer", defaultRefer)
}

type statusEnvelope struct {
	Status struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	} `json:"status"`
	Data json.RawMessage `json:"data"`
}

type Profile struct {
	Email string
	OUID  string
	VIP   string
	RegTS int64
}

func (c *Client) FetchProfile(ctx context.Context, account Account) (Profile, error) {
	account = account.normalized()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/oreate/user/getuserinfo"), nil)
	if err != nil {
		return Profile{}, err
	}
	setHeaders(req, account, "application/json")
	resp, err := c.httpClient(true).Do(req)
	if err != nil {
		return Profile{}, fmt.Errorf("%w: profile request: %v", ErrTemporaryUpstream, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Profile{}, ErrAuth
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return Profile{}, err
	}
	var env statusEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Profile{}, fmt.Errorf("%w: invalid profile response", ErrTemporaryUpstream)
	}
	if env.Status.Code != 0 {
		return Profile{}, classifyUpstreamError(env.Status.Code, env.Status.Msg)
	}
	var data struct {
		BasicInfo struct {
			Email      string `json:"email"`
			CreateTime int64  `json:"createTime"`
		} `json:"basicInfo"`
		VIPInfo struct {
			VIPType any `json:"vipType"`
		} `json:"vipInfo"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return Profile{}, fmt.Errorf("%w: invalid profile data", ErrTemporaryUpstream)
	}
	return Profile{
		Email: strings.TrimSpace(data.BasicInfo.Email),
		OUID:  account.OUID,
		VIP:   scalarString(data.VIPInfo.VIPType, "0"),
		RegTS: data.BasicInfo.CreateTime,
	}, nil
}

func (c *Client) FetchCreditsBalance(ctx context.Context, account Account) (map[string]any, error) {
	account = account.normalized()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/oreate/account/getpointdetail"), nil)
	if err != nil {
		return nil, err
	}
	setHeaders(req, account, "application/json")
	resp, err := c.httpClient(true).Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: credits request: %v", ErrTemporaryUpstream, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrAuth
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	var env statusEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("%w: invalid credits response", ErrTemporaryUpstream)
	}
	if env.Status.Code != 0 {
		return nil, classifyUpstreamError(env.Status.Code, env.Status.Msg)
	}
	var buckets map[string]*struct {
		Amount  int   `json:"amount"`
		EndTime int64 `json:"endTime"`
	}
	if err := json.Unmarshal(env.Data, &buckets); err != nil {
		return nil, fmt.Errorf("%w: invalid credits data", ErrTemporaryUpstream)
	}
	remaining := 0
	var nextExpiry int64
	for _, bucket := range buckets {
		if bucket == nil {
			continue
		}
		remaining += bucket.Amount
		if bucket.Amount > 0 && bucket.EndTime > 0 && (nextExpiry == 0 || bucket.EndTime < nextExpiry) {
			nextExpiry = bucket.EndTime
		}
	}
	result := map[string]any{
		"remaining": remaining,
		"total":     remaining,
		"used":      0,
		"unknown":   false,
		"error":     nil,
	}
	if nextExpiry > 0 {
		result["reset_after"] = time.Unix(nextExpiry, 0).UTC().Format(time.RFC3339)
	}
	return result, nil
}

func scalarString(v any, fallback string) string {
	switch x := v.(type) {
	case string:
		if strings.TrimSpace(x) != "" {
			return strings.TrimSpace(x)
		}
	case float64:
		return strconv.FormatInt(int64(x), 10)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case json.Number:
		return x.String()
	}
	return fallback
}

func cookieValue(cookie, name string) string {
	for _, part := range strings.Split(cookie, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && key == name {
			return value
		}
	}
	return ""
}

// mergeCookies overlays browser-observed cookies onto the stored cookie so the
// website request carries the full tracking jar; stored values win on conflict.
func mergeCookies(stored, browser string) string {
	merged := strings.TrimSpace(stored)
	for _, part := range strings.Split(browser, ";") {
		name, _, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || name == "" || cookieValue(merged, name) != "" {
			continue
		}
		merged += "; " + strings.TrimSpace(part)
	}
	return merged
}

func IsOreateCookie(cookie string) bool {
	return cookieValue(cookie, "OUID") != "" && cookieValue(cookie, "ouss") != ""
}
