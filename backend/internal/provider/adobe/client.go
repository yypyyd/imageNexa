package adobe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/google/uuid"
)

const (
	submitURL       = "https://firefly-3p.ff.adobe.io/v2/3p-images/generate-async"
	image5SubmitURL = "https://image-v5.ff.adobe.io/v1/images/generate-async"
	videoSubmitURL  = "https://firefly-3p.ff.adobe.io/v2/3p-videos/generate-async"
	// Firefly-native video model (project id "firefly-video"): distinct host,
	// submit path and storage host from the 3p (veo/luma) video flow.
	fireflyVideoSubmitURL   = "https://video-v1.ff.adobe.io/v2/videos/generate"
	fireflyVideoStorageBase = "https://video-v1.ff.adobe.io/v2/storage"
	storageBase             = "https://firefly-3p.ff.adobe.io/v2/storage"
	creditsURL              = "https://firefly.adobe.io/v1/credits/balance"
	creditsAPIKey           = "SunbreakWebUI1"
)

var (
	ErrAuth              = errors.New("adobe auth failed")
	ErrQuotaExhausted    = errors.New("adobe quota exhausted")
	ErrTemporaryUpstream = errors.New("adobe upstream temporary error")
	ErrDeadUpstream      = errors.New("adobe upstream fatal error")
	// ErrContentRejected is Adobe's content-safety filter refusing the prompt or
	// the generated image (HTTP 451 image_unsafe). It is the prompt's fault, not
	// the account's — every account rejects the same content — so the caller must
	// surface it as-is without penalizing/killing the account or failing over.
	ErrContentRejected = errors.New("内容安全审核未通过，请修改提示词或参考素材后重试")
)

// ContentRejectionError preserves Adobe's moderation code so callers can
// distinguish a blocked prompt/reference from a randomly unsafe generated
// image. All variants unwrap to ErrContentRejected and therefore never affect
// account health or trigger account failover.
type ContentRejectionError struct {
	Code string
}

func (e *ContentRejectionError) Error() string {
	switch e.Code {
	case "image_unsafe":
		return "Adobe 生成结果未通过安全审核"
	case "prompt_unsafe":
		return "Adobe 提示词未通过内容安全审核，请修改提示词后重试"
	case "reference_image_privacy_error":
		return "Adobe 内容安全审核未通过：参考图片包含真人面部，Adobe 不允许将其用于生成内容，请更换不含真人面部的图片"
	default:
		return ErrContentRejected.Error()
	}
}

func (e *ContentRejectionError) Unwrap() error { return ErrContentRejected }

func contentRejectionError(status int, body string) error {
	code := contentRejectionCode(status, body)
	if code == "" {
		return nil
	}
	return &ContentRejectionError{Code: code}
}

func contentRejectionCode(status int, body string) string {
	if status != 451 {
		return ""
	}
	var payload any
	if json.Unmarshal([]byte(body), &payload) == nil {
		if code := findAdobeErrorCode(payload); code != "" {
			code = strings.ToLower(strings.TrimSpace(code))
			if strings.HasSuffix(code, "_unsafe") || code == "reference_image_privacy_error" {
				return code
			}
			return ""
		}
	}
	lowerBody := strings.ToLower(body)
	for _, code := range []string{"reference_image_privacy_error", "prompt_unsafe", "image_unsafe"} {
		if strings.Contains(lowerBody, code) {
			return code
		}
	}
	if strings.Contains(lowerBody, "unsafe") {
		return "unsafe"
	}
	return ""
}

func findAdobeErrorCode(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range []string{"error_code", "errorCode", "code"} {
			if code, ok := typed[key].(string); ok && strings.TrimSpace(code) != "" {
				return code
			}
		}
		for _, child := range typed {
			if code := findAdobeErrorCode(child); code != "" {
				return code
			}
		}
	case []any:
		for _, child := range typed {
			if code := findAdobeErrorCode(child); code != "" {
				return code
			}
		}
	}
	return ""
}

func isRetryableGeneratedImageRejection(err error) bool {
	var rejection *ContentRejectionError
	return errors.As(err, &rejection) && rejection.Code == "image_unsafe"
}

var profileURLs = []string{
	"https://ims-na1.adobelogin.com/ims/profile/v1",
	"https://adobeid-na1.services.adobe.com/ims/profile/v1",
}

type Client struct {
	apiKey  string
	proxyMu sync.RWMutex
	proxy   string
}

func NewClient(apiKey, proxy string) *Client {
	return &Client{
		apiKey: defaultString(apiKey, clientID),
		proxy:  strings.TrimSpace(proxy),
	}
}

func (c *Client) proxyValue() string {
	c.proxyMu.RLock()
	defer c.proxyMu.RUnlock()
	return c.proxy
}

func (c *Client) ExchangeCookie(ctx context.Context, cookie string) (*CookieExchangeResult, error) {
	sess, err := c.newProxyTLSClient()
	if err != nil {
		return nil, err
	}
	return exchangeCookieWithTLSClient(ctx, sess, cookie)
}

// uploadMaxRetries is how many extra in-place attempts a transient upload
// failure (transport error / timeout, 429/451/5xx) gets on a fresh connection
// before the error is surfaced.
const uploadMaxRetries = 5

// UploadImage stores a reference image and returns its blob id. Transient
// failures are retried in place (uploadMaxRetries times); the final error keeps
// its original (non-temporary) classification so the account is not penalized
// for a network blip.
func (c *Client) UploadImage(ctx context.Context, token string, content []byte, contentType, engine string) (string, error) {
	return c.UploadMedia(ctx, token, content, contentType, engine, "image")
}

// UploadMedia stores an image, video, or audio reference and returns the blob
// id used by the video-generation request.
func (c *Client) UploadMedia(ctx context.Context, token string, content []byte, contentType, engine, kind string) (string, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind != "image" && kind != "video" && kind != "audio" {
		return "", errors.New("unsupported adobe media kind")
	}
	body, err, retryable := c.uploadMediaOnce(ctx, token, content, contentType, engine, kind)
	for attempt := 0; err != nil && retryable && attempt < uploadMaxRetries && ctx.Err() == nil; attempt++ {
		body, err, retryable = c.uploadMediaOnce(ctx, token, content, contentType, engine, kind)
	}
	if err != nil {
		return "", err
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	if id := uploadedMediaID(payload); id != "" {
		return id, nil
	}
	return "", errors.New("adobe upload missing blob id")
}

func uploadedMediaID(payload map[string]any) string {
	for _, key := range []string{"id", "blobId", "blob_id"} {
		if id := strings.TrimSpace(stringValue(payload[key])); id != "" {
			return id
		}
	}
	for _, key := range []string{"images", "assets", "videos", "audios", "data"} {
		switch value := payload[key].(type) {
		case []any:
			for _, item := range value {
				if child, ok := item.(map[string]any); ok {
					if id := uploadedMediaID(child); id != "" {
						return id
					}
				}
			}
		case map[string]any:
			if id := uploadedMediaID(value); id != "" {
				return id
			}
		}
	}
	return ""
}

// uploadImageOnce performs a single upload attempt and returns the raw response
// body plus whether a failure is retryable (transport error / 429/451/5xx).
// Auth failures (401/403) and other non-200s are not retryable.
func (c *Client) uploadMediaOnce(ctx context.Context, token string, content []byte, contentType, engine, kind string) ([]byte, error, bool) {
	sess, err := c.newDirectTLSClient()
	if err != nil {
		return nil, err, false
	}

	base := storageBase
	if engine == "firefly-video" {
		base = fireflyVideoStorageBase
	}
	endpoint := base + "/" + kind
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(content))
	if err != nil {
		return nil, err, false
	}
	req = req.WithContext(ctx)
	req.Header = http.Header{
		"authorization": {"Bearer " + strings.TrimSpace(token)},
		"x-api-key":     {c.apiKey},
		"content-type":  {defaultString(contentType, defaultMediaContentType(kind))},
		"accept":        {"*/*"},
		"user-agent":    {sess.fp.userAgent},
		http.HeaderOrderKey: {
			"authorization",
			"x-api-key",
			"content-type",
			"accept",
			"user-agent",
		},
	}

	resp, err := sess.client.Do(req)
	if err != nil {
		// Transport error (incl. Client.Timeout on the storage endpoint) — a
		// network blip, retryable on a fresh connection.
		return nil, fmt.Errorf("adobe upload request: %w", err), true
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err, true
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("%w (upload %d %s: %s)", ErrAuth, resp.StatusCode, resp.Header.Get("x-access-error"), clip(body, 300)), false
	}
	if resp.StatusCode == 429 || resp.StatusCode == 451 || resp.StatusCode >= 500 {
		return nil, fmt.Errorf("adobe upload failed: %d %s", resp.StatusCode, clip(body, 300)), true
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("adobe upload failed: %d %s", resp.StatusCode, clip(body, 300)), false
	}
	return body, nil, true
}

func defaultMediaContentType(kind string) string {
	switch kind {
	case "video":
		return "video/mp4"
	case "audio":
		return "audio/mpeg"
	default:
		return "image/png"
	}
}

const generatedImageSafetyMaxAttempts = 3

func (c *Client) GenerateImage(ctx context.Context, token, modelID, prompt, aspectRatio, resolution string, blobIDs []string, downloadResult bool) ([]byte, map[string]any, error) {
	submitSess, err := c.newSubmitTLSClient()
	if err != nil {
		return nil, nil, err
	}
	pollSess, err := c.newDirectTLSClient()
	if err != nil {
		return nil, nil, err
	}
	downloadSess, err := c.newDirectTLSClient()
	if err != nil {
		return nil, nil, err
	}

	var lastBody []byte
	var lastErr error
generationAttempts:
	for attempt := 0; attempt < generatedImageSafetyMaxAttempts; attempt++ {
		// Rebuild payloads on every output-safety retry so seed-bearing models get
		// a genuinely different sample. Firefly Image 5 uses a separate schema.
		endpoint := submitURL
		var candidates []map[string]any
		if modelID == "firefly-image-5" {
			endpoint = image5SubmitURL
			candidates = []map[string]any{buildImage5Payload(prompt, aspectRatio, resolution, blobIDs)}
		} else {
			candidates = BuildImagePayloadCandidates(modelID, prompt, aspectRatio, resolution, blobIDs)
		}
		for _, payload := range candidates {
			respBody, pollURL, submitErr := c.submitImage(ctx, submitSess, token, prompt, endpoint, payload)
			if submitErr == nil {
				meta, data, pollErr := c.pollImage(ctx, pollSess, downloadSess, token, pollURL, downloadResult)
				if isRetryableGeneratedImageRejection(pollErr) {
					lastErr = pollErr
					continue generationAttempts
				}
				if pollErr != nil {
					return nil, nil, pollErr
				}
				return data, meta, nil
			}
			lastBody = respBody
			lastErr = submitErr
			if isRetryableGeneratedImageRejection(submitErr) {
				continue generationAttempts
			}
			if errors.Is(submitErr, ErrAuth) || errors.Is(submitErr, ErrQuotaExhausted) || errors.Is(submitErr, ErrContentRejected) {
				return nil, nil, submitErr
			}
		}
		break
	}
	if isRetryableGeneratedImageRejection(lastErr) {
		return nil, nil, fmt.Errorf("%w：Adobe 生成结果连续 %d 次未通过安全审核，请补充成年人、着装和场景描述后重试", ErrContentRejected, generatedImageSafetyMaxAttempts)
	}
	// Preserve the temporary classification so the pool retries (overload / 5xx /
	// rate-limit) instead of failing the request outright.
	if errors.Is(lastErr, ErrDeadUpstream) {
		return nil, nil, fmt.Errorf("%w: adobe submit: %s", ErrDeadUpstream, clip(lastBody, 300))
	}
	if errors.Is(lastErr, ErrTemporaryUpstream) {
		return nil, nil, fmt.Errorf("%w: adobe submit: %s", ErrTemporaryUpstream, clip(lastBody, 300))
	}
	return nil, nil, fmt.Errorf("adobe submit failed: %s", clip(lastBody, 300))
}

// GenerateVideo renders the clip and (when downloadResult) downloads the MP4.
// With downloadResult=false it returns nil bytes and the upstream presigned URL
// in meta["video_url"] — used by the async /v1/videos job, which proxies that URL
// on /content instead of persisting the file.
func (c *Client) GenerateVideo(ctx context.Context, token, engine, prompt, aspectRatio string, durationSeconds int, resolution, referenceMode, upstreamModel string, inputs VideoInputs, downloadResult bool) ([]byte, map[string]any, error) {
	submitSess, err := c.newSubmitTLSClient()
	if err != nil {
		return nil, nil, err
	}
	pollSess, err := c.newDirectTLSClient()
	if err != nil {
		return nil, nil, err
	}
	downloadSess, err := c.newDirectTLSClient()
	if err != nil {
		return nil, nil, err
	}

	payload := BuildVideoPayload(engine, prompt, aspectRatio, durationSeconds, resolution, referenceMode, upstreamModel, inputs)
	endpoint := videoSubmitURL
	if engine == "firefly-video" {
		endpoint = fireflyVideoSubmitURL
	}
	respBody, pollURL, err := c.submitVideo(ctx, submitSess, token, endpoint, payload)
	if err != nil {
		return nil, nil, err
	}
	_ = respBody
	meta, data, pollErr := c.pollVideo(ctx, pollSess, downloadSess, token, pollURL, downloadResult)
	if pollErr != nil {
		return nil, nil, pollErr
	}
	return data, meta, nil
}

func (c *Client) FetchAccountProfile(ctx context.Context, token string) (map[string]any, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return map[string]any{}, nil
	}
	sess, err := c.newProxyTLSClient()
	if err != nil {
		return nil, err
	}

	for _, rawURL := range profileURLs {
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req = req.WithContext(ctx)
		req.Header = http.Header{
			"authorization": {"Bearer " + token},
			"accept":        {"application/json"},
			"user-agent":    {sess.fp.userAgent},
			http.HeaderOrderKey: {
				"authorization",
				"accept",
				"user-agent",
			},
		}

		resp, err := sess.client.Do(req)
		if err != nil {
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil || resp.StatusCode != 200 {
			continue
		}

		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			continue
		}
		email := strings.TrimSpace(stringValue(payload["email"]))
		displayName := strings.TrimSpace(stringValue(payload["displayName"]))
		if displayName == "" {
			displayName = strings.TrimSpace(stringValue(payload["name"]))
		}
		if displayName == "" {
			displayName = strings.TrimSpace(stringValue(payload["fullName"]))
		}
		userID := strings.TrimSpace(stringValue(payload["userId"]))
		if userID == "" {
			userID = strings.TrimSpace(stringValue(payload["authId"]))
		}
		if email != "" || displayName != "" || userID != "" {
			return map[string]any{
				"email":        emptyStringNil(email),
				"display_name": emptyStringNil(displayName),
				"user_id":      emptyStringNil(userID),
			}, nil
		}
	}

	return map[string]any{}, nil
}

func (c *Client) FetchCreditsBalance(ctx context.Context, token string) (map[string]any, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return map[string]any{
			"remaining":       nil,
			"used":            nil,
			"total":           nil,
			"available_until": nil,
			"unknown":         true,
			"error":           "empty token",
		}, nil
	}

	accountID := ExtractAccountID(token)
	if accountID == "" {
		return map[string]any{
			"remaining":       nil,
			"used":            nil,
			"total":           nil,
			"available_until": nil,
			"unknown":         true,
			"error":           "no account id",
		}, nil
	}

	sess, err := c.newProxyTLSClient()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, creditsURL, nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header = http.Header{
		"authorization": {"Bearer " + token},
		"x-api-key":     {creditsAPIKey},
		"x-account-id":  {accountID},
		"accept":        {"application/json"},
		"content-type":  {"application/json"},
		"user-agent":    {sess.fp.userAgent},
		http.HeaderOrderKey: {
			"authorization",
			"x-api-key",
			"x-account-id",
			"accept",
			"content-type",
			"user-agent",
		},
	}

	resp, err := sess.client.Do(req)
	if err != nil {
		return map[string]any{
			"remaining":       nil,
			"used":            nil,
			"total":           nil,
			"available_until": nil,
			"unknown":         true,
			"error":           "network: " + err.Error(),
		}, nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 401 {
		return nil, ErrAuth
	}
	if resp.StatusCode != 200 {
		return map[string]any{
			"remaining":       nil,
			"used":            nil,
			"total":           nil,
			"available_until": nil,
			"unknown":         true,
			"error":           fmt.Sprintf("http %d: %s", resp.StatusCode, clip(body, 160)),
		}, nil
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return map[string]any{
			"remaining":       nil,
			"used":            nil,
			"total":           nil,
			"available_until": nil,
			"unknown":         true,
			"error":           "non-json",
		}, nil
	}

	totalInfo, _ := payload["total"].(map[string]any)
	quota, _ := totalInfo["quota"].(map[string]any)
	return map[string]any{
		"remaining":       intOrNil(quota["available"]),
		"used":            intOrNil(quota["used"]),
		"total":           intOrNil(quota["total"]),
		"available_until": emptyStringNil(strings.TrimSpace(stringValue(totalInfo["availableUntil"]))),
		"unknown":         false,
		"error":           nil,
	}, nil
}

func (c *Client) submitImage(ctx context.Context, sess *tlsSession, token, prompt, endpoint string, payload map[string]any) ([]byte, string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req = req.WithContext(ctx)
	req.Header = http.Header{
		"authorization":      {"Bearer " + strings.TrimSpace(token)},
		"x-api-key":          {c.apiKey},
		"content-type":       {"application/json"},
		"accept":             {"*/*"},
		"origin":             {"https://firefly.adobe.com"},
		"referer":            {"https://firefly.adobe.com/"},
		"accept-language":    {"en-US,en;q=0.9"},
		"sec-ch-ua":          {sess.fp.secCHUA},
		"sec-ch-ua-mobile":   {"?0"},
		"sec-ch-ua-platform": {sess.fp.platform},
		// firefly.adobe.com and *.ff.adobe.io have different registrable
		// domains, so Chrome classifies this CORS request as cross-site.
		"sec-fetch-site": {"cross-site"},
		"sec-fetch-mode": {"cors"},
		"sec-fetch-dest": {"empty"},
		"user-agent":     {sess.fp.userAgent},
		http.HeaderOrderKey: {
			"authorization",
			"x-api-key",
			"content-type",
			"accept",
			"origin",
			"referer",
			"accept-language",
			"sec-ch-ua",
			"sec-ch-ua-mobile",
			"sec-ch-ua-platform",
			"sec-fetch-site",
			"sec-fetch-mode",
			"sec-fetch-dest",
			"user-agent",
			"x-nonce",
		},
	}
	if nonce := buildSubmitNonce(token, prompt); nonce != "" {
		req.Header.Set("x-nonce", nonce)
	}

	resp, err := sess.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrTemporaryUpstream, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		if strings.EqualFold(resp.Header.Get("x-access-error"), "taste_exhausted") {
			return respBody, "", ErrQuotaExhausted
		}
		return respBody, "", fmt.Errorf("%w (submit %d %s: %s)", ErrAuth, resp.StatusCode, resp.Header.Get("x-access-error"), clip(respBody, 300))
	}
	// Adobe may report overload in a nominal response. It remains a regular
	// temporary upstream error: there is no client-side breaker or submit pacing.
	if b := string(respBody); strings.Contains(b, "system under load") || strings.Contains(b, "timeout_error") {
		return respBody, "", fmt.Errorf("%w: adobe submit: %s", ErrTemporaryUpstream, clip(respBody, 300))
	}
	if rejectErr := contentRejectionError(resp.StatusCode, string(respBody)); rejectErr != nil {
		return respBody, "", rejectErr
	}
	if resp.StatusCode == 429 || resp.StatusCode == 451 || resp.StatusCode >= 500 {
		return respBody, "", ErrDeadUpstream
	}
	if resp.StatusCode != 200 {
		return respBody, "", errors.New("submit rejected")
	}

	var payloadResp map[string]any
	if err := json.Unmarshal(respBody, &payloadResp); err != nil {
		return respBody, "", err
	}
	if override := strings.TrimSpace(resp.Header.Get("x-override-status-link")); override != "" {
		return respBody, override, nil
	}
	if links, ok := payloadResp["links"].(map[string]any); ok {
		if result, ok := links["result"].(map[string]any); ok {
			if href := strings.TrimSpace(stringValue(result["href"])); href != "" {
				return respBody, href, nil
			}
		}
		if href := strings.TrimSpace(stringValue(links["result"])); href != "" {
			return respBody, href, nil
		}
	}
	return respBody, "", errors.New("submit ok but no poll url")
}

func (c *Client) pollImage(ctx context.Context, sess, downloadSess *tlsSession, token, pollURL string, downloadResult bool) (map[string]any, []byte, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, fmt.Errorf("adobe generation timed out: %w", err)
		}

		req, err := http.NewRequest(http.MethodGet, pollURL, nil)
		if err != nil {
			return nil, nil, err
		}
		req = req.WithContext(ctx)
		req.Header = http.Header{
			"authorization": {"Bearer " + strings.TrimSpace(token)},
			"accept":        {"*/*"},
			"origin":        {"https://firefly.adobe.com"},
			"referer":       {"https://firefly.adobe.com/"},
			"user-agent":    {sess.fp.userAgent},
			http.HeaderOrderKey: {
				"authorization",
				"accept",
				"origin",
				"referer",
				"user-agent",
			},
		}

		resp, err := sess.client.Do(req)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrTemporaryUpstream, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, nil, readErr
		}
		if b := string(body); strings.Contains(b, "system under load") || strings.Contains(b, "timeout_error") {
			return nil, nil, ErrTemporaryUpstream
		}
		if rejectErr := contentRejectionError(resp.StatusCode, string(body)); rejectErr != nil {
			return nil, nil, rejectErr
		}
		if resp.StatusCode == 429 || resp.StatusCode == 451 || resp.StatusCode >= 500 {
			return nil, nil, fmt.Errorf("%w (poll %d: %s)", ErrDeadUpstream, resp.StatusCode, clip(body, 300))
		}
		if resp.StatusCode != 200 {
			return nil, nil, fmt.Errorf("adobe poll failed: %d %s", resp.StatusCode, clip(body, 300))
		}

		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, nil, err
		}
		if outputs, ok := payload["outputs"].([]any); ok && len(outputs) > 0 {
			if first, ok := outputs[0].(map[string]any); ok {
				if image, ok := first["image"].(map[string]any); ok {
					if url := strings.TrimSpace(stringValue(image["presignedUrl"])); url != "" {
						// Expose the presigned URL for callers that want it directly.
						payload["image_url"] = url
						if !downloadResult {
							return payload, nil, nil
						}
						data, err := c.download(ctx, downloadSess, url)
						if err != nil {
							return nil, nil, err
						}
						return payload, data, nil
					}
				}
			}
		}

		status := strings.ToUpper(strings.TrimSpace(stringValue(payload["status"])))
		if status == "FAILED" || status == "CANCELLED" || status == "ERROR" {
			return nil, nil, fmt.Errorf("adobe job failed: %s", clip(body, 300))
		}
		time.Sleep(3 * time.Second)
	}
}

func (c *Client) submitVideo(ctx context.Context, sess *tlsSession, token, endpoint string, payload map[string]any) ([]byte, string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req = req.WithContext(ctx)
	req.Header = http.Header{
		"authorization":      {"Bearer " + strings.TrimSpace(token)},
		"x-api-key":          {c.apiKey},
		"content-type":       {"application/json"},
		"accept":             {"*/*"},
		"origin":             {"https://firefly.adobe.com"},
		"referer":            {"https://firefly.adobe.com/"},
		"accept-language":    {"en-US,en;q=0.9"},
		"sec-ch-ua":          {sess.fp.secCHUA},
		"sec-ch-ua-mobile":   {"?0"},
		"sec-ch-ua-platform": {sess.fp.platform},
		// firefly.adobe.com and *.ff.adobe.io have different registrable
		// domains, so Chrome classifies this CORS request as cross-site.
		"sec-fetch-site": {"cross-site"},
		"sec-fetch-mode": {"cors"},
		"sec-fetch-dest": {"empty"},
		"user-agent":     {sess.fp.userAgent},
		http.HeaderOrderKey: {
			"authorization",
			"x-api-key",
			"content-type",
			"accept",
			"origin",
			"referer",
			"accept-language",
			"sec-ch-ua",
			"sec-ch-ua-mobile",
			"sec-ch-ua-platform",
			"sec-fetch-site",
			"sec-fetch-mode",
			"sec-fetch-dest",
			"user-agent",
			"x-nonce",
		},
	}
	// The working video submit (HAR) carries x-nonce just like the image submit.
	if prompt, _ := payload["prompt"].(string); prompt != "" {
		if nonce := buildSubmitNonce(token, prompt); nonce != "" {
			req.Header.Set("x-nonce", nonce)
		}
	}

	resp, err := sess.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrTemporaryUpstream, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		if strings.EqualFold(resp.Header.Get("x-access-error"), "taste_exhausted") {
			return respBody, "", ErrQuotaExhausted
		}
		// Surface Adobe's response body — "adobe auth failed" alone hides whether
		// it's a bad token, a missing scope, or a WAF/fingerprint block.
		return respBody, "", fmt.Errorf("%w (%d %s: %s)", ErrAuth, resp.StatusCode, resp.Header.Get("x-access-error"), clip(respBody, 300))
	}
	if rejectErr := contentRejectionError(resp.StatusCode, string(respBody)); rejectErr != nil {
		return respBody, "", rejectErr
	}
	if resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode == 451 || resp.StatusCode >= 500 {
		// Keep the status and clipped body while preserving errors.Is semantics.
		// Partner-model discovery can report DEGRADED versions whose submit path
		// fails with the same broad status family as malformed model payloads; the
		// response detail is required to tell those cases apart safely.
		return respBody, "", fmt.Errorf("%w (submit %d: %s)", ErrDeadUpstream, resp.StatusCode, clip(respBody, 300))
	}
	// "system under load" / timeout_error = adobe overload — treat as a temporary
	// error so the tempFailover policy moves to the next account (same as the image path).
	if b := string(respBody); strings.Contains(b, "system under load") || strings.Contains(b, "timeout_error") {
		return respBody, "", fmt.Errorf("%w: adobe submit: %s", ErrTemporaryUpstream, clip(respBody, 300))
	}
	if resp.StatusCode != 200 {
		return respBody, "", fmt.Errorf("video submit rejected: %d %s", resp.StatusCode, clip(respBody, 300))
	}

	var payloadResp map[string]any
	if err := json.Unmarshal(respBody, &payloadResp); err != nil {
		return respBody, "", err
	}
	if override := strings.TrimSpace(resp.Header.Get("x-override-status-link")); override != "" {
		return respBody, normalizeVideoPollURL(override), nil
	}
	if links, ok := payloadResp["links"].(map[string]any); ok {
		if result, ok := links["result"].(map[string]any); ok {
			if href := strings.TrimSpace(stringValue(result["href"])); href != "" {
				return respBody, normalizeVideoPollURL(href), nil
			}
		}
		if href := strings.TrimSpace(stringValue(links["result"])); href != "" {
			return respBody, normalizeVideoPollURL(href), nil
		}
	}
	return respBody, "", errors.New("video submit ok but no poll url")
}

func (c *Client) pollVideo(ctx context.Context, sess, downloadSess *tlsSession, token, pollURL string, downloadResult bool) (map[string]any, []byte, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, fmt.Errorf("adobe video generation timed out: %w", err)
		}

		req, err := http.NewRequest(http.MethodGet, pollURL, nil)
		if err != nil {
			return nil, nil, err
		}
		req = req.WithContext(ctx)
		req.Header = http.Header{
			"authorization": {"Bearer " + strings.TrimSpace(token)},
			"accept":        {"*/*"},
			"origin":        {"https://firefly.adobe.com"},
			"referer":       {"https://firefly.adobe.com/"},
			"user-agent":    {sess.fp.userAgent},
			http.HeaderOrderKey: {
				"authorization",
				"accept",
				"origin",
				"referer",
				"user-agent",
			},
		}

		resp, err := sess.client.Do(req)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrTemporaryUpstream, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, nil, readErr
		}
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return nil, nil, fmt.Errorf("%w (%d %s: %s)", ErrAuth, resp.StatusCode, resp.Header.Get("x-access-error"), clip(body, 300))
		}
		if b := string(body); strings.Contains(b, "system under load") || strings.Contains(b, "timeout_error") {
			return nil, nil, ErrTemporaryUpstream
		}
		if rejectErr := contentRejectionError(resp.StatusCode, string(body)); rejectErr != nil {
			return nil, nil, rejectErr
		}
		if resp.StatusCode == 429 || resp.StatusCode == 451 || resp.StatusCode >= 500 {
			return nil, nil, fmt.Errorf("%w (poll %d: %s)", ErrDeadUpstream, resp.StatusCode, clip(body, 300))
		}
		if resp.StatusCode != 200 {
			return nil, nil, fmt.Errorf("adobe video poll failed: %d %s", resp.StatusCode, clip(body, 300))
		}

		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, nil, err
		}
		if outputs, ok := payload["outputs"].([]any); ok && len(outputs) > 0 {
			if first, ok := outputs[0].(map[string]any); ok {
				if video, ok := first["video"].(map[string]any); ok {
					if raw := strings.TrimSpace(stringValue(video["presignedUrl"])); raw != "" {
						payload["video_url"] = raw
						if !downloadResult {
							return payload, nil, nil
						}
						data, err := c.download(ctx, downloadSess, raw)
						if err != nil {
							return nil, nil, err
						}
						return payload, data, nil
					}
				}
			}
		}

		status := strings.ToUpper(strings.TrimSpace(stringValue(payload["status"])))
		if status == "FAILED" || status == "CANCELLED" || status == "ERROR" {
			return nil, nil, fmt.Errorf("adobe video job failed: %s", clip(body, 300))
		}
		time.Sleep(3 * time.Second)
	}
}

// download fetches the generated artifact from Adobe's presigned S3 URL. The
// asset is already produced at this point, so a transient network hiccup
// (connection EOF/reset mid-body, S3 accelerate 5xx) must not fail the whole
// generation — retry with backoff before giving up.
const downloadTimeout = 3 * time.Minute

func (c *Client) download(parent context.Context, sess *tlsSession, url string) ([]byte, error) {
	// The artifact is already produced; detach from the (often nearly-elapsed)
	// generation ctx deadline/cancel and give the download its own budget so a
	// slow CDN read isn't killed mid-body and the backoff-retries can actually run.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), downloadTimeout)
	defer cancel()
	var data []byte
	var err error
	for _, wait := range []time.Duration{0, 1 * time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second} {
		if wait > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
		data, err = c.downloadOnce(ctx, sess, url)
		if err == nil || !errors.Is(err, ErrTemporaryUpstream) {
			break
		}
	}
	return data, err
}

func (c *Client) downloadOnce(ctx context.Context, sess *tlsSession, url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header = http.Header{
		"accept":     {"*/*"},
		"user-agent": {sess.fp.userAgent},
		http.HeaderOrderKey: {
			"accept",
			"user-agent",
		},
	}
	resp, err := sess.client.Do(req)
	if err != nil {
		// Network-level failure (EOF, reset, timeout) — transient, worth a retry.
		return nil, fmt.Errorf("%w: %v", ErrTemporaryUpstream, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 500 {
			return nil, fmt.Errorf("%w: adobe download failed: %d %s", ErrTemporaryUpstream, resp.StatusCode, clip(body, 200))
		}
		return nil, fmt.Errorf("adobe download failed: %d %s", resp.StatusCode, clip(body, 200))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		// Body truncated mid-read (EOF) — transient, retry.
		return nil, fmt.Errorf("%w: read body: %v", ErrTemporaryUpstream, err)
	}
	return data, nil
}

// fingerprint bundles a TLS ClientProfile with the browser headers that match
// it, so the JA3/JA4 handshake and the advertised User-Agent / client hints
// stay internally consistent within a single request.
type fingerprint struct {
	profile   profiles.ClientProfile
	userAgent string
	secCHUA   string
	platform  string // quoted sec-ch-ua-platform value, e.g. `"Windows"`
}

func newChromeFingerprint(profile profiles.ClientProfile, major int) fingerprint {
	ua := fmt.Sprintf("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36", major)
	platform := `"Windows"`
	return fingerprint{
		profile:   profile,
		userAgent: ua,
		secCHUA:   fmt.Sprintf(`"Not=A?Brand";v="99", "Google Chrome";v="%d", "Chromium";v="%d"`, major, major),
		platform:  platform,
	}
}

// Adobe correlates IMS exchange and generation traffic as one browser session.
// Rotating the advertised browser version and operating system on every HTTP
// client made a single access token appear to jump between unrelated devices,
// which the 3P gateway masks as timeout_error / "system under load". Keep one
// modern, internally consistent Chrome identity across the complete account
// lifecycle. Chrome itself still randomizes TLS extension order below.
var adobeBrowserFingerprint = newChromeFingerprint(profiles.Chrome_146, 146)

func currentAdobeFingerprint() fingerprint {
	return adobeBrowserFingerprint
}

// tlsSession pairs a configured TLS client with the fingerprint it was built
// for, so downstream header builders advertise the matching UA / client hints.
type tlsSession struct {
	client tlsclient.HttpClient
	fp     fingerprint
}

// newProxyTLSClient is used for authentication, account maintenance, and
// generation-submit requests. Large media and artifact transfers use direct.
func (c *Client) newProxyTLSClient() (*tlsSession, error) {
	return c.newTLSSession(currentAdobeFingerprint(), true)
}

func (c *Client) newSubmitTLSClient() (*tlsSession, error) { return c.newProxyTLSClient() }

func (c *Client) newDirectTLSClient() (*tlsSession, error) {
	return c.newTLSSession(currentAdobeFingerprint(), false)
}

func (c *Client) newTLSSession(fp fingerprint, useProxy bool) (*tlsSession, error) {
	options := []tlsclient.HttpClientOption{
		tlsclient.WithTimeoutSeconds(60),
		tlsclient.WithClientProfile(fp.profile),
		tlsclient.WithNotFollowRedirects(),
		tlsclient.WithRandomTLSExtensionOrder(),
	}
	if proxy := c.proxyValue(); useProxy && proxy != "" {
		options = append(options, tlsclient.WithProxyUrl(proxy))
	}
	client, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), options...)
	if err != nil {
		return nil, err
	}
	return &tlsSession{client: client, fp: fp}, nil
}

func exchangeCookieWithTLSClient(ctx context.Context, sess *tlsSession, cookie string) (*CookieExchangeResult, error) {
	cookie = normalizeCookie(cookie)
	if cookie == "" {
		return nil, ErrAdobeCookieEmpty
	}

	// Adobe's current IMS library binds each cookie exchange to a browser
	// fingerprint request. Omitting this field still returns a token, but leaves
	// the resulting session without the request correlation used by Firefly's 3P
	// gateway. Match the current web client and send the client hints belonging to
	// the same TLS/User-Agent profile as well.
	body := buildCookieExchangeForm()
	req, err := http.NewRequest(http.MethodPost, refreshURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}

	req = req.WithContext(ctx)
	req.Header = http.Header{
		"accept":             {"*/*"},
		"accept-language":    {"zh-CN,zh;q=0.9"},
		"content-type":       {"application/x-www-form-urlencoded;charset=UTF-8"},
		"cookie":             {cookie},
		"origin":             {"https://firefly.adobe.com"},
		"referer":            {"https://firefly.adobe.com/"},
		"sec-ch-ua":          {sess.fp.secCHUA},
		"sec-ch-ua-mobile":   {"?0"},
		"sec-ch-ua-platform": {sess.fp.platform},
		"user-agent":         {sess.fp.userAgent},
		http.HeaderOrderKey: {
			"accept",
			"accept-language",
			"content-type",
			"cookie",
			"origin",
			"referer",
			"sec-ch-ua",
			"sec-ch-ua-mobile",
			"sec-ch-ua-platform",
			"user-agent",
		},
	}
	resp, err := sess.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("adobe cookie exchange network error: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("adobe cookie exchange upstream %d: %s", resp.StatusCode, clip(respBody, 200))
	}
	var payload map[string]any
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return nil, fmt.Errorf("adobe cookie exchange invalid json: %w", err)
	}
	token := strings.TrimSpace(stringValue(payload["access_token"]))
	if token == "" {
		return nil, errors.New("adobe cookie exchange missing access_token")
	}
	return &CookieExchangeResult{
		AccessToken: token,
		ExpiresIn:   intValue(payload["expires_in"]),
		Raw:         payload,
	}, nil
}

func buildCookieExchangeForm() string {
	return url.Values{
		"client_id":              {clientID},
		"guest_allowed":          {"true"},
		"scope":                  {scopeValue},
		"fingerprint_request_id": {uuid.NewString()},
	}.Encode()
}

func buildSubmitNonce(token, prompt string) string {
	claims := decodeJWTPayload(token)
	userID := strings.TrimSpace(stringValue(claims["user_id"]))
	if userID == "" {
		userID = strings.TrimSpace(stringValue(claims["aa_id"]))
	}
	if userID == "" {
		userID = strings.TrimSpace(stringValue(claims["sub"]))
	}
	prompt = strings.TrimSpace(prompt)
	if userID == "" || prompt == "" {
		return ""
	}
	if len(prompt) > 256 {
		prompt = prompt[:256]
	}
	sum := sha256.Sum256([]byte(userID + "-" + prompt))
	return hex.EncodeToString(sum[:])
}

func ExtractAccountID(token string) string {
	claims := decodeJWTPayload(token)
	userID := strings.TrimSpace(stringValue(claims["user_id"]))
	if userID == "" {
		userID = strings.TrimSpace(stringValue(claims["aa_id"]))
	}
	if userID == "" {
		userID = strings.TrimSpace(stringValue(claims["sub"]))
	}
	return userID
}

func normalizeVideoPollURL(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host := parsed.Hostname()
	if !strings.HasPrefix(host, "firefly-epo") {
		return raw
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) == 0 {
		return raw
	}
	jobID := strings.TrimSpace(parts[len(parts)-1])
	hostSuffix := strings.TrimPrefix(host, "firefly-epo")
	hostSuffix = strings.SplitN(hostSuffix, ".", 2)[0]
	if len(hostSuffix) != 4 {
		return raw
	}
	for _, ch := range hostSuffix {
		if ch < '0' || ch > '9' {
			return raw
		}
	}
	return "https://bks-epo" + hostSuffix + ".adobe.io/v2/jobs/result/" + jobID + "?host=" + parsed.Host + "/"
}

func clip(v []byte, n int) string {
	s := strings.TrimSpace(string(v))
	if len(s) <= n {
		return s
	}
	return s[:n]
}
