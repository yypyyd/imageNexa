package grok

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"
	"os"
	"regexp"
	"strings"
	"time"

	"backend/internal/netguard"

	http "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/google/uuid"
)

// assetBase is where generated media artifacts live (the stream returns a
// path like "users/<uid>/generated/<id>/generated_video.mp4").
const assetBase = "https://assets.grok.com/"

// Grok may leave the conversations/new stream open while a longer (10s) clip
// continues rendering. Do not hold the request until the HTTP client's 600s
// timeout; after this window GenerateVideo falls back to polling the
// deterministic generated_video.mp4 asset URL.
const grokVideoStreamWait = 90 * time.Second

var videoURLRe = regexp.MustCompile(`"videoUrl":"([^"]+)"`)

// GenerateVideo runs grok's imagine video pipeline:
//  1. POST /rest/media/post/create  -> a media post id (parentPostId)
//  2. POST /rest/app-chat/conversations/new with modelName "imagine-video-gen"
//     and a videoGenModelConfig referencing that post id; the streaming response
//     reports progress and, at completion, the artifact videoUrl.
//
// frames (optional, up to the model's max) enable image-to-video: each image is
// uploaded to grok and referenced as an imageReference. aspectRatio is passed
// through ("9:16" etc.); resolution is the tier ("720p"); seconds is the clip
// length (6 or 10). When downloadResult is false, returns nil bytes and the
// artifact URL in meta["video_url"]; otherwise downloads the mp4.
func (c *Client) GenerateVideo(ctx context.Context, token, prompt, aspectRatio, resolution string, seconds int, frames [][]byte, downloadResult bool) ([]byte, map[string]any, error) {
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	if token == "" {
		return nil, nil, ErrAuth
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, nil, fmt.Errorf("grok: prompt required")
	}
	if strings.TrimSpace(aspectRatio) == "" {
		aspectRatio = "16:9"
	}
	if strings.TrimSpace(resolution) == "" {
		resolution = "720p"
	}
	if seconds != 6 && seconds != 10 && seconds != 15 {
		seconds = 10
	}

	// Account bootstrap/challenge and generation submits use the proxy; reference
	// uploads, polling, and artifact downloads remain direct.
	submitClient, err := c.newSubmitTLSClient()
	if err != nil {
		return nil, nil, err
	}
	directClient, err := c.newDirectTLSClient()
	if err != nil {
		return nil, nil, err
	}

	// Self-heal the x-statsig-id anti-bot challenge for THIS session before the
	// gated submit. Without it the header falls back to the static defaults,
	// which go stale on a new grok web build and make conversations/new answer
	// 403 (anti-bot). Challenge refresh itself is an upstream bootstrap request;
	// failure is non-fatal (statsigID then uses defaults).
	c.ensureChallenge(ctx, submitClient, token)

	// Image-to-video: upload each reference frame directly and collect its asset URL.
	var imageRefs []string
	for _, f := range frames {
		if len(f) == 0 {
			continue
		}
		url, upErr := c.uploadImage(ctx, directClient, token, f)
		if upErr != nil {
			return nil, nil, upErr
		}
		imageRefs = append(imageRefs, url)
	}

	// grok streams progress inline, but the stream is fragile: it regularly dies
	// mid-body ("unexpected EOF") after only the conversation object / an early
	// progress event — while grok keeps rendering the clip server-side. The
	// rendered clip always lands at the deterministic URL
	// assets.grok.com/users/{userId}/generated/{postId}/generated_video.mp4, so a
	// dead stream is NOT a failed generation: poll that URL until the clip is
	// ready instead of failing the job (which would also fail over and burn
	// another account's credits for a video that already exists).
	// The parent post is part of the gated generation submit. It must use the
	// same proxied session/IP as ensureChallenge and conversations/new; mixing a
	// proxied challenge with a direct create request triggers Cloudflare's HTML
	// challenge even when the dynamic Statsig signature is valid.
	postID, userID, err := c.createPost(ctx, submitClient, token, prompt)
	if err != nil {
		return nil, nil, err
	}

	videoCfg := map[string]any{
		"parentPostId":       postID,
		"aspectRatio":        aspectRatio,
		"videoLength":        seconds,
		"resolutionName":     resolution,
		"isReferenceToVideo": len(imageRefs) > 0,
	}
	if len(imageRefs) > 0 {
		videoCfg["imageReferences"] = imageRefs
	}
	payload := map[string]any{
		"temporary":        true,
		"modelName":        "imagine-video-gen",
		"message":          prompt + " --mode=custom",
		"enableSideBySide": true,
		"responseMetadata": map[string]any{
			"modelConfigOverride": map[string]any{
				"modelMap": map[string]any{"videoGenModelConfig": videoCfg},
			},
		},
	}

	// body may be partial when psErr != nil — still worth scanning.
	streamCtx, cancelStream := context.WithTimeout(ctx, grokVideoStreamWait)
	body, psErr := c.postStream(streamCtx, submitClient, token, "/rest/app-chat/conversations/new", payload)
	cancelStream()
	if dir := strings.TrimSpace(os.Getenv("GROK_DUMP")); dir != "" {
		_ = os.WriteFile(dir+"/grok_body_"+postID+".txt", []byte(body), 0o644)
	}
	// Out-of-credits surfaces as a stream error (HTTP is still 200) — not retryable.
	if strings.Contains(body, "usagePoolExhausted") || strings.Contains(body, "media generation credits") {
		return nil, nil, fmt.Errorf("%w: media generation credits exhausted", ErrQuotaExhausted)
	}
	if psErr != nil && (errors.Is(psErr, ErrAuth) || errors.Is(psErr, ErrQuotaExhausted)) {
		return nil, nil, psErr
	}
	// A fatal stream error means grok definitively rejected this generation
	// (content moderation, an unsupported parameter, etc.). Retrying — on this
	// or any other account — fails identically and only burns the pool, so fail
	// fast with a non-temporary error the pool won't fail over on.
	if strings.Contains(body, "STREAM_ERROR_SEVERITY_FATAL") {
		return nil, nil, fmt.Errorf("grok: video generation rejected by upstream (fatal stream error): %s", clip([]byte(body), 200))
	}
	// Fast path: the artifact appears as "videoUrl":"users/.../generated_video.mp4".
	artifact := ""
	for _, m := range videoURLRe.FindAllStringSubmatch(body, -1) {
		if v := strings.TrimSpace(m[1]); v != "" {
			artifact = v // keep the last (progress=100) one
		}
	}
	if artifact == "" && userID != "" {
		// Stream ended without the artifact (dropped connection or early close):
		// the render continues upstream — wait for the deterministic asset URL.
		url := fmt.Sprintf("%susers/%s/generated/%s/generated_video.mp4", assetBase, userID, postID)
		if werr := c.waitForAsset(ctx, directClient, token, url); werr == nil {
			artifact = url
		} else if errors.Is(werr, ErrAuth) || errors.Is(werr, context.Canceled) || errors.Is(werr, context.DeadlineExceeded) {
			return nil, nil, werr
		}
	}
	if artifact == "" {
		if psErr != nil {
			return nil, nil, psErr
		}
		return nil, nil, fmt.Errorf("%w: no video artifact in response: %s", ErrTemporaryUpstream, clip([]byte(body), 200))
	}
	fullURL := artifact
	if !strings.HasPrefix(fullURL, "http") {
		fullURL = assetBase + strings.TrimPrefix(artifact, "/")
	}

	meta := map[string]any{
		"provider":  "grok",
		"post_id":   postID,
		"video_url": fullURL,
	}
	if !downloadResult {
		return nil, meta, nil
	}
	data, err := c.download(ctx, directClient, token, fullURL)
	if err != nil {
		return nil, nil, err
	}
	return data, meta, nil
}

// uploadImage uploads one reference frame via /http/upload-file-v2/direct and
// returns its asset content URL for imageReferences. Cloudflare's bot score is
// per-request, so retry transient failures with backoff instead of failing the
// whole task.
func (c *Client) uploadImage(ctx context.Context, client tlsclient.HttpClient, token string, img []byte) (string, error) {
	var res map[string]any
	var err error
	backoffs := []time.Duration{0, 2 * time.Second, 5 * time.Second, 10 * time.Second}
	for _, wait := range backoffs {
		if wait > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(wait):
			}
		}
		res, err = c.uploadFileV2(ctx, client, token, img)
		if err == nil || !errors.Is(err, ErrTemporaryUpstream) {
			break
		}
	}
	if err != nil {
		return "", err
	}
	meta, _ := res["fileMetadata"].(map[string]any)
	fileURI := ""
	if meta != nil {
		fileURI = strings.TrimSpace(stringValue(meta["fileUri"]))
	}
	if fileURI == "" {
		return "", fmt.Errorf("%w: upload missing fileUri", ErrTemporaryUpstream)
	}
	if strings.HasPrefix(fileURI, "http") {
		return fileURI, nil
	}
	return assetBase + strings.TrimPrefix(fileURI, "/"), nil
}

func (c *Client) uploadFileV2(ctx context.Context, client tlsclient.HttpClient, token string, img []byte) (map[string]any, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	partHeader := textproto.MIMEHeader{}
	partHeader.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, uuid.NewString()+".png"))
	partHeader.Set("Content-Type", "image/png")
	part, err := mw.CreatePart(partHeader)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(img); err != nil {
		return nil, err
	}
	if err := mw.WriteField("file_source", "IMAGINE_SELF_UPLOAD_FILE_SOURCE"); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, apiBase+"/http/upload-file-v2/direct", bytes.NewReader(buf.Bytes()))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	c.applyHeaders(req, token, map[string]string{"content-type": mw.FormDataContentType()})

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTemporaryUpstream, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTemporaryUpstream, err)
	}
	if e := mapStatus("/http/upload-file-v2/direct", resp.StatusCode, raw); e != nil {
		return nil, e
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: upload non-json: %s", ErrTemporaryUpstream, clip(raw, 120))
	}
	return out, nil
}

// createPost registers a video media post and returns its id (parentPostId).
// createPost creates the parent media post. grok uses its id as the videoId,
// and the rendered clip is stored under users/{userId}/generated/{postId}/.
func (c *Client) createPost(ctx context.Context, client tlsclient.HttpClient, token, prompt string) (postID, userID string, err error) {
	res, err := c.postJSON(ctx, client, token, "/rest/media/post/create", map[string]any{
		"mediaType": "MEDIA_POST_TYPE_VIDEO",
		"prompt":    prompt,
	})
	if err != nil {
		return "", "", err
	}
	post, _ := res["post"].(map[string]any)
	postID = strings.TrimSpace(stringValue(post["id"]))
	userID = strings.TrimSpace(stringValue(post["userId"]))
	if postID == "" {
		return "", "", fmt.Errorf("%w: media post missing id", ErrTemporaryUpstream)
	}
	return postID, userID, nil
}

// waitForAsset polls a generated-asset URL with ranged direct GETs until Grok
// finishes rendering it; assets.grok.com is not anti-bot gated.
// 404 means "still rendering"; auth failures abort.
func (c *Client) waitForAsset(ctx context.Context, client tlsclient.HttpClient, token, url string) error {
	deadline := time.Now().Add(6 * time.Minute)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req = req.WithContext(ctx)
		req.Header = http.Header{
			"user-agent": {userAgent},
			"referer":    {origin + "/"},
			"cookie":     {"sso=" + token + "; sso-rw=" + token},
			"range":      {"bytes=0-0"},
		}
		if resp, derr := client.Do(req); derr == nil {
			code := resp.StatusCode
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			switch {
			case code == http.StatusOK || code == http.StatusPartialContent:
				return nil
			case code == http.StatusUnauthorized || code == http.StatusForbidden:
				return fmt.Errorf("%w: asset %d", ErrAuth, code)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: video render did not complete in time", ErrTemporaryUpstream)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(4 * time.Second):
		}
	}
}

// postJSON does an authed JSON POST and parses a single JSON object response.
func (c *Client) postJSON(ctx context.Context, client tlsclient.HttpClient, token, path string, body any) (map[string]any, error) {
	raw, err := c.postWithChallengeRetry(ctx, client, token, path, body)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: %s non-json: %s", ErrTemporaryUpstream, path, clip(raw, 120))
	}
	return out, nil
}

// postStream does an authed JSON POST and returns the full (streamed) text body.
func (c *Client) postStream(ctx context.Context, client tlsclient.HttpClient, token, path string, body any) (string, error) {
	raw, err := c.postWithChallengeRetry(ctx, client, token, path, body)
	return string(raw), err
}

// postWithChallengeRetry refreshes and replays exactly once when a protected
// submit is rejected before acceptance. A second rejection preserves
// ErrChallenge so the service can fail fast instead of rotating every account.
func (c *Client) postWithChallengeRetry(ctx context.Context, client tlsclient.HttpClient, token, path string, body any) ([]byte, error) {
	attempt := func() ([]byte, error) {
		raw, status, err := c.doPost(ctx, client, token, path, body)
		if err != nil {
			return raw, err
		}
		return raw, mapStatus(path, status, raw)
	}
	return retryChallengeOnce(ctx, attempt, func(ctx context.Context) error {
		return c.refreshChallenge(ctx, client)
	})
}

func retryChallengeOnce(ctx context.Context, attempt func() ([]byte, error), refresh func(context.Context) error) ([]byte, error) {
	raw, err := attempt()
	if !errors.Is(err, ErrChallenge) {
		return raw, err
	}
	if refreshErr := refresh(ctx); refreshErr != nil {
		return raw, fmt.Errorf("%w: refresh failed: %v", ErrChallenge, refreshErr)
	}
	return attempt()
}

func (c *Client) doPost(ctx context.Context, client tlsclient.HttpClient, token, path string, body any) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(http.MethodPost, apiBase+path, reader)
	if err != nil {
		return nil, 0, err
	}
	req = req.WithContext(ctx)
	c.applyHeaders(req, token, nil)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrTemporaryUpstream, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		// Mid-body stream drops ("unexpected EOF", HTTP/2 resets) are common on
		// long renders — return what was received so callers can still use it.
		return raw, resp.StatusCode, fmt.Errorf("%w: %v", ErrTemporaryUpstream, err)
	}
	return raw, resp.StatusCode, nil
}

// OpenAsset streams a grok asset (e.g. a generated video) authenticated with the
// account token — used by the async /v1/videos /content proxy. The caller MUST
// close the returned ReadCloser.
func (c *Client) OpenAsset(ctx context.Context, token, url string) (io.ReadCloser, string, error) {
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	if token == "" {
		return nil, "", ErrAuth
	}
	allowedHosts := []string{"grok.com", "x.ai"}
	current, err := netguard.ValidateAssetURL(ctx, url, allowedHosts)
	if err != nil {
		return nil, "", fmt.Errorf("%w: rejected asset URL", ErrTemporaryUpstream)
	}
	originalHost := strings.ToLower(current.Hostname())
	// Artifact streaming is a non-submit request and therefore stays direct.
	client, err := c.newAssetTLSClient(allowedHosts)
	if err != nil {
		return nil, "", err
	}
	for redirects := 0; redirects <= netguard.MaxRedirects; redirects++ {
		req, requestErr := http.NewRequest(http.MethodGet, current.String(), nil)
		if requestErr != nil {
			return nil, "", requestErr
		}
		req = req.WithContext(ctx)
		req.Header = http.Header{"user-agent": {userAgent}}
		credentialSent := strings.EqualFold(current.Hostname(), originalHost)
		if credentialSent {
			req.Header.Set("referer", origin+"/")
			req.Header.Set("cookie", "sso="+token+"; sso-rw="+token)
		}
		resp, doErr := client.Do(req)
		if doErr != nil {
			return nil, "", fmt.Errorf("%w: %v", ErrTemporaryUpstream, doErr)
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			location := resp.Header.Get("Location")
			resp.Body.Close()
			if redirects == netguard.MaxRedirects || location == "" {
				return nil, "", fmt.Errorf("%w: invalid asset redirect", ErrTemporaryUpstream)
			}
			current, err = netguard.ResolveRedirect(ctx, current, location, allowedHosts)
			if err != nil {
				return nil, "", fmt.Errorf("%w: rejected asset redirect", ErrTemporaryUpstream)
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			if credentialSent && (resp.StatusCode == 401 || resp.StatusCode == 403) {
				return nil, "", fmt.Errorf("%w: asset %d", ErrAuth, resp.StatusCode)
			}
			return nil, "", fmt.Errorf("%w: asset %d", ErrTemporaryUpstream, resp.StatusCode)
		}
		body, contentType, guardErr := netguard.GuardStream(resp.Body, resp.Header.Get("Content-Type"), resp.ContentLength, netguard.MediaAny, netguard.MaxVideoBytes)
		if guardErr != nil {
			return nil, "", fmt.Errorf("%w: invalid media asset", ErrTemporaryUpstream)
		}
		return body, contentType, nil
	}
	return nil, "", fmt.Errorf("%w: asset redirect limit", ErrTemporaryUpstream)
}

// download fetches the rendered artifact. The clip is already generated at this
// point, so a transient failure here (HTTP/2 stream reset, CF hiccup) must not
// fail the whole task — retry with backoff.
func (c *Client) download(ctx context.Context, client tlsclient.HttpClient, token, url string) ([]byte, error) {
	var data []byte
	var err error
	for _, wait := range []time.Duration{0, 2 * time.Second, 5 * time.Second, 10 * time.Second} {
		if wait > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
		data, err = c.downloadOnce(ctx, client, token, url)
		if err == nil || !errors.Is(err, ErrTemporaryUpstream) {
			break
		}
	}
	return data, err
}

func (c *Client) downloadOnce(ctx context.Context, client tlsclient.HttpClient, token, url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header = http.Header{
		"user-agent": {userAgent},
		"referer":    {origin + "/"},
		"cookie":     {"sso=" + token + "; sso-rw=" + token},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTemporaryUpstream, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: download %d", ErrTemporaryUpstream, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTemporaryUpstream, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty video download", ErrTemporaryUpstream)
	}
	return data, nil
}

// mapStatus maps an HTTP status to the shared provider error sentinels.
func mapStatus(path string, status int, raw []byte) error {
	switch {
	case status == 200:
		return nil
	case status == 403 && (isStatsigProtectedPath(path) || isBotChallenge(string(raw))):
		// grok bot-detection or a Cloudflare challenge page ("Just a moment…"),
		// NOT a dead token — transient, so a good account isn't killed by an
		// IP/anti-bot hiccup. Invalidate even a TTL-fresh snapshot so the next
		// request refreshes the homepage challenge and signer chunk through the
		// normal HTTP/Goja path.
		invalidateStatsigChallenge()
		return fmt.Errorf("%w: %s 403 %s", ErrChallenge, path, clip(raw, 160))
	case status == 401 || status == 403:
		return fmt.Errorf("%w: %s %d %s", ErrAuth, path, status, clip(raw, 160))
	case status == 429:
		// 429 is grok RATE-LIMITING ("Too many requests") — a transient error that
		// must NOT kill the account. Only a body that names a credit/usage-pool
		// exhaustion is a real quota wall.
		if isCreditError(string(raw)) {
			return fmt.Errorf("%w: %s 429 %s", ErrQuotaExhausted, path, clip(raw, 160))
		}
		return fmt.Errorf("%w: %s 429 %s", ErrTemporaryUpstream, path, clip(raw, 160))
	case status >= 500:
		return fmt.Errorf("%w: %s %d %s", ErrTemporaryUpstream, path, status, clip(raw, 160))
	default:
		if isCreditError(string(raw)) {
			return fmt.Errorf("%w: %s", ErrQuotaExhausted, clip(raw, 160))
		}
		return fmt.Errorf("grok: %s %d %s", path, status, clip(raw, 160))
	}
}

func isStatsigProtectedPath(path string) bool {
	switch path {
	case "/rest/app-chat/conversations/new", "/rest/media/post/create":
		return true
	default:
		return false
	}
}

// isBotChallenge reports whether a 403 body is an anti-bot interstitial rather
// than a real auth rejection: grok's own "anti-bot" marker or a Cloudflare
// challenge page ("Just a moment…" / cf-chl / challenge-platform).
func isBotChallenge(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "anti-bot") ||
		strings.Contains(s, "page is out of date") ||
		strings.Contains(s, "reload to continue") ||
		strings.Contains(s, "just a moment") ||
		strings.Contains(s, "cf-chl") ||
		strings.Contains(s, "challenge-platform") ||
		strings.Contains(s, "cf_chl")
}

func isCreditError(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "usagepoolexhausted") || strings.Contains(s, "credit") || strings.Contains(s, "insufficient") || strings.Contains(s, "quota")
}
