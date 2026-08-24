// Package custom implements a generic OpenAI-compatible upstream client. A
// "custom" model forwards generation to any OpenAI-compatible API: the upstream
// base_url + api_key live on a custom account (pool="custom"), the upstream model
// name on the model config (UpstreamModel). Custom upstream traffic always uses
// direct local egress and never inherits process-level proxy environment variables.
package custom

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	ErrAuth              = errors.New("custom upstream auth failed")
	ErrQuotaExhausted    = errors.New("custom upstream quota exhausted")
	ErrTemporaryUpstream = errors.New("custom upstream temporary error")
	ErrBadRequest        = errors.New("custom upstream rejected request")
)

type Client struct {
	http *http.Client
}

func NewClient() *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Custom upstreams are administrator-configured direct routes. Explicitly
	// ignore HTTP_PROXY/HTTPS_PROXY instead of inheriting process environment.
	transport.Proxy = nil
	return &Client{http: &http.Client{Transport: transport, Timeout: 10 * time.Minute}}
}

// sanitizeErr strips the upstream URL/host from a network error so a user's
// private upstream URL never leaks into the event log / API response.
func sanitizeErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "context deadline exceeded"), strings.Contains(s, "Client.Timeout"), strings.Contains(s, "timeout"):
		return "request timeout"
	case strings.Contains(s, "connection refused"):
		return "connection refused"
	case strings.Contains(s, "no such host"), strings.Contains(s, "dial tcp"), strings.Contains(s, "lookup "):
		return "cannot reach upstream"
	case strings.Contains(s, "tls"), strings.Contains(s, "TLS"), strings.Contains(s, "certificate"):
		return "TLS error"
	case strings.Contains(s, "EOF"), strings.Contains(s, "reset by peer"), strings.Contains(s, "broken pipe"):
		return "connection reset"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return strings.ToLower(ue.Op) + " upstream failed"
	}
	return "upstream request failed"
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	return c.http.Do(req)
}

// ChatResponse is a successful OpenAI-compatible chat-completions response.
// Streaming bodies remain connected to the upstream and must be closed by the
// caller. Non-streaming bodies are validated and buffered before return so an
// HTTP-200 business error can safely fail over to another custom account.
type ChatResponse struct {
	Header http.Header
	Body   io.ReadCloser
	Stream bool
}

// ChatCompletions forwards a request to the upstream OpenAI-compatible chat
// endpoint. The caller-facing model name is replaced with upstreamModel while
// every other request field is preserved verbatim.
func (c *Client) ChatCompletions(ctx context.Context, baseURL, apiKey, upstreamModel string, payload []byte, stream bool) (*ChatResponse, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || strings.TrimSpace(apiKey) == "" {
		return nil, ErrAuth
	}
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		return nil, fmt.Errorf("%w: invalid json", ErrBadRequest)
	}
	body["model"] = upstreamModel
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid request", ErrBadRequest)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrTemporaryUpstream, sanitizeErr(err))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
		return nil, mapStatus(resp.StatusCode, errBody)
	}

	if !stream {
		defer resp.Body.Close()
		responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20+1))
		if err != nil {
			return nil, fmt.Errorf("%w: response read failed", ErrTemporaryUpstream)
		}
		if len(responseBody) > 32<<20 {
			return nil, fmt.Errorf("%w: response too large", ErrTemporaryUpstream)
		}
		if !validChatCompletion(responseBody) {
			return nil, fmt.Errorf("%w: bad chat response: %s", ErrTemporaryUpstream, clip(responseBody, 160))
		}
		return &ChatResponse{Header: resp.Header.Clone(), Body: io.NopCloser(bytes.NewReader(responseBody))}, nil
	}

	// Read through the first SSE data event before exposing downstream headers.
	// This catches HTTP-200 JSON business errors and empty streams while failover
	// is still safe. The validated prefix is replayed to the downstream unchanged.
	reader := bufio.NewReader(resp.Body)
	prefix, err := readValidSSEPrefix(reader)
	if err != nil {
		resp.Body.Close()
		return nil, err
	}
	return &ChatResponse{
		Header: resp.Header.Clone(),
		Body:   &prefixedReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), reader), Closer: resp.Body},
		Stream: true,
	}, nil
}

type prefixedReadCloser struct {
	io.Reader
	io.Closer
}

func validChatCompletion(raw []byte) bool {
	var out struct {
		Object  string            `json:"object"`
		Choices []json.RawMessage `json:"choices"`
		Error   any               `json:"error"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Error != nil || len(out.Choices) == 0 {
		return false
	}
	return out.Object == "" || out.Object == "chat.completion"
}

func readValidSSEPrefix(r *bufio.Reader) ([]byte, error) {
	var prefix bytes.Buffer
	for prefix.Len() <= 256<<10 {
		line, err := r.ReadBytes('\n')
		prefix.Write(line)
		if len(line) > 0 {
			trimmed := strings.TrimSpace(string(line))
			if strings.HasPrefix(trimmed, "data:") {
				data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				if data == "[DONE]" {
					return nil, fmt.Errorf("%w: stream ended before first completion chunk", ErrTemporaryUpstream)
				}
				var chunk struct {
					Object  string            `json:"object"`
					Choices []json.RawMessage `json:"choices"`
					Error   any               `json:"error"`
				}
				if json.Unmarshal([]byte(data), &chunk) == nil && chunk.Error == nil && len(chunk.Choices) > 0 &&
					(chunk.Object == "" || chunk.Object == "chat.completion.chunk") {
					return prefix.Bytes(), nil
				}
				return nil, fmt.Errorf("%w: bad stream response: %s", ErrTemporaryUpstream, clip([]byte(data), 160))
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("%w: empty chat stream", ErrTemporaryUpstream)
			}
			return nil, fmt.Errorf("%w: stream read failed", ErrTemporaryUpstream)
		}
	}
	return nil, fmt.Errorf("%w: stream prelude too large", ErrTemporaryUpstream)
}

// GenerateImage calls the upstream OpenAI image API. With reference images it
// uses /v1/images/edits (multipart); otherwise /v1/images/generations. Returns
// the raw image bytes (decoded from b64_json, or downloaded from url).
func (c *Client) GenerateImage(ctx context.Context, baseURL, apiKey, model, prompt, size, quality string, refs [][]byte, downloadResult bool) ([]byte, string, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || apiKey == "" {
		return nil, "", ErrAuth
	}
	var req *http.Request
	var err error
	if len(refs) > 0 {
		body := &bytes.Buffer{}
		w := multipart.NewWriter(body)
		_ = w.WriteField("model", model)
		_ = w.WriteField("prompt", prompt)
		// Ask the upstream for a URL (not base64) so we can pass it through directly.
		_ = w.WriteField("response_format", "url")
		if size != "" {
			_ = w.WriteField("size", size)
		}
		if quality != "" {
			_ = w.WriteField("quality", quality)
		}
		for i, r := range refs {
			fw, e := w.CreateFormFile("image[]", fmt.Sprintf("ref_%d.png", i+1))
			if e != nil {
				return nil, "", e
			}
			_, _ = fw.Write(r)
		}
		_ = w.Close()
		req, err = http.NewRequest(http.MethodPost, baseURL+"/v1/images/edits", body)
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Content-Type", w.FormDataContentType())
	} else {
		payload := map[string]any{"model": model, "prompt": prompt, "n": 1, "response_format": "url"}
		if size != "" {
			payload["size"] = size
		}
		if quality != "" {
			payload["quality"] = quality
		}
		raw, _ := json.Marshal(payload)
		req, err = http.NewRequest(http.MethodPost, baseURL+"/v1/images/generations", bytes.NewReader(raw))
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Content-Type", "application/json")
	}
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := c.do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %s", ErrTemporaryUpstream, sanitizeErr(err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if e := mapStatus(resp.StatusCode, body); e != nil {
		return nil, "", e
	}
	return c.imageFromResponse(ctx, body, downloadResult)
}

// GenerateVideo drives the upstream Sora-style async video API:
// POST /v1/videos → poll GET /v1/videos/{id} → GET /v1/videos/{id}/content.
// Reference frames (image-to-video / first-last frames) are sent as multipart
// input_reference[] files, matching the OpenAI videos API. When downloadResult
// is false it returns the upstream content URL instead.
func (c *Client) GenerateVideo(ctx context.Context, baseURL, apiKey, model, prompt, size string, seconds int, frames [][]byte, downloadResult bool) ([]byte, string, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || apiKey == "" {
		return nil, "", ErrAuth
	}
	var created map[string]any
	var err error
	if len(frames) > 0 {
		body := &bytes.Buffer{}
		w := multipart.NewWriter(body)
		_ = w.WriteField("model", model)
		_ = w.WriteField("prompt", prompt)
		if size != "" {
			_ = w.WriteField("size", size)
		}
		if seconds > 0 {
			_ = w.WriteField("seconds", fmt.Sprintf("%d", seconds))
		}
		for i, f := range frames {
			fw, e := w.CreateFormFile("input_reference[]", fmt.Sprintf("frame_%d.png", i+1))
			if e != nil {
				return nil, "", e
			}
			_, _ = fw.Write(f)
		}
		_ = w.Close()
		created, err = c.submitMultipart(ctx, baseURL+"/v1/videos", apiKey, body, w.FormDataContentType())
	} else {
		payload := map[string]any{"model": model, "prompt": prompt}
		if size != "" {
			payload["size"] = size
		}
		if seconds > 0 {
			payload["seconds"] = fmt.Sprintf("%d", seconds)
		}
		raw, _ := json.Marshal(payload)
		created, err = c.submitJSON(ctx, http.MethodPost, baseURL+"/v1/videos", apiKey, raw)
	}
	if err != nil {
		return nil, "", err
	}
	jobID := strings.TrimSpace(stringValue(created["id"]))
	if jobID == "" {
		return nil, "", fmt.Errorf("%w: video create missing id", ErrTemporaryUpstream)
	}
	// Poll until terminal.
	for {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		job, err := c.doJSON(ctx, http.MethodGet, baseURL+"/v1/videos/"+jobID, apiKey, nil)
		if err != nil {
			if errors.Is(err, ErrTemporaryUpstream) {
				if sleepCtx(ctx, 5*time.Second) != nil {
					return nil, "", ctx.Err()
				}
				continue
			}
			return nil, "", err
		}
		switch strings.ToLower(strings.TrimSpace(stringValue(job["status"]))) {
		case "completed", "succeeded", "success":
			contentURL := baseURL + "/v1/videos/" + jobID + "/content"
			if !downloadResult {
				return nil, contentURL, nil
			}
			data, err := c.download(ctx, contentURL, apiKey)
			if err != nil {
				return nil, "", err
			}
			return data, contentURL, nil
		case "failed", "error", "canceled", "cancelled":
			reason := stringValue(job["error"])
			if isCreditError(reason) {
				return nil, "", fmt.Errorf("%w: %s", ErrTemporaryUpstream, clip([]byte(reason), 160))
			}
			return nil, "", fmt.Errorf("custom: video %s", clip([]byte(reason), 160))
		}
		if sleepCtx(ctx, 5*time.Second) != nil {
			return nil, "", ctx.Err()
		}
	}
}

func (c *Client) submitJSON(ctx context.Context, method, url, apiKey string, body []byte) (map[string]any, error) {
	return c.doJSON(ctx, method, url, apiKey, body)
}

func (c *Client) doJSON(ctx context.Context, method, url, apiKey string, body []byte) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrTemporaryUpstream, sanitizeErr(err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if e := mapStatus(resp.StatusCode, raw); e != nil {
		return nil, e
	}
	var out map[string]any
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: non-json: %s", ErrTemporaryUpstream, clip(raw, 120))
	}
	return out, nil
}

func (c *Client) submitMultipart(ctx context.Context, url, apiKey string, body io.Reader, contentType string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", contentType)
	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrTemporaryUpstream, sanitizeErr(err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if e := mapStatus(resp.StatusCode, raw); e != nil {
		return nil, e
	}
	var out map[string]any
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: non-json: %s", ErrTemporaryUpstream, clip(raw, 120))
	}
	return out, nil
}

func (c *Client) download(ctx context.Context, url, apiKey string) ([]byte, error) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrTemporaryUpstream, sanitizeErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: download %d", ErrTemporaryUpstream, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty download", ErrTemporaryUpstream)
	}
	return data, nil
}

// imageBytesFromResponse extracts image bytes from an OpenAI images response:
// data[0].b64_json (preferred) or data[0].url (downloaded).
// imageFromResponse parses an OpenAI image response and returns the upstream URL.
// We always request response_format=url, so the response must carry a URL — a
// base64-only response is treated as an error (no base64 pass-through). With
// downloadResult=false the URL is returned directly (no download).
func (c *Client) imageFromResponse(ctx context.Context, body []byte, downloadResult bool) ([]byte, string, error) {
	var out struct {
		Data []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil || len(out.Data) == 0 {
		return nil, "", fmt.Errorf("%w: bad image response: %s", ErrTemporaryUpstream, clip(body, 160))
	}
	url := strings.TrimSpace(out.Data[0].URL)
	if url == "" {
		return nil, "", fmt.Errorf("%w: image response had no url (upstream ignored response_format=url)", ErrTemporaryUpstream)
	}
	if !downloadResult {
		return nil, url, nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := c.do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %s", ErrTemporaryUpstream, sanitizeErr(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return raw, url, err
}

func mapStatus(status int, body []byte) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == 401 || status == 403:
		return fmt.Errorf("%w: %d %s", ErrAuth, status, clip(body, 160))
	case status == 429:
		// Custom upstreams have NO "quota exhausted" lock — a 429 is just rate
		// limiting, treated as a temporary error (fail over, account stays active).
		return fmt.Errorf("%w: 429 %s", ErrTemporaryUpstream, clip(body, 160))
	case status >= 500:
		return fmt.Errorf("%w: %d %s", ErrTemporaryUpstream, status, clip(body, 160))
	default:
		if isCreditError(string(body)) {
			return fmt.Errorf("%w: %s", ErrTemporaryUpstream, clip(body, 160))
		}
		return fmt.Errorf("%w: %d %s", ErrBadRequest, status, clip(body, 160))
	}
}

func isCreditError(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "insufficient") || strings.Contains(s, "quota") ||
		strings.Contains(s, "credit") || strings.Contains(s, "balance")
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

func clip(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n]
	}
	return s
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
