package runway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	mrand "math/rand/v2"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
)

// ratioDimensions maps an aspect ratio to the Gen-4 Turbo native output size.
// These are the only dimensions gen4_turbo accepts; "2K" is a UI label over this
// native tier (see runway-video-gen-spec). Unknown ratios fall back to 16:9.
func ratioDimensions(aspectRatio string) (int, int) {
	switch strings.TrimSpace(strings.ReplaceAll(aspectRatio, "x", ":")) {
	case "16:9":
		return 1280, 720
	case "9:16":
		return 720, 1280
	case "1:1":
		return 960, 960
	case "4:3":
		return 1104, 832
	case "3:4":
		return 832, 1104
	case "21:9":
		return 1584, 672
	default:
		return 1280, 720
	}
}

// GenerateVideo runs the full i2v pipeline (gen_video.py): upload the first-frame
// image (preview + dataset), create a dataset, create a gen4_turbo task and poll
// it to completion, then download the rendered MP4. teamID is the workspace id
// (meta["team_id"]); if empty it's derived from the token. seconds must be 5 or
// 10; aspectRatio picks the native output size.
// GenerateVideo renders the clip and (when downloadResult) downloads the MP4.
// With downloadResult=false it returns nil bytes and the upstream artifact URL in
// meta["video_url"] — used by the async /v1/videos job, which proxies that URL on
// /content instead of persisting the file.
func (c *Client) GenerateVideo(ctx context.Context, token, teamID, prompt, aspectRatio string, seconds int, frame []byte, downloadResult bool) ([]byte, map[string]any, error) {
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	if token == "" {
		return nil, nil, ErrAuth
	}
	if teamID == "" {
		teamID = TeamIDFromToken(token)
	}
	if teamID == "" {
		return nil, nil, errors.New("runway: no team id")
	}
	if len(frame) == 0 {
		return nil, nil, errors.New("runway: first-frame image required")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(frame))
	if err != nil {
		return nil, nil, errors.New("runway: failed to decode first-frame image")
	}

	// Account quota and /v1/tasks generation submit use the proxy; project setup,
	// reference uploads, polling, and downloads remain direct.
	submitClient, err := c.newSubmitTLSClient()
	if err != nil {
		return nil, nil, err
	}
	directClient, err := c.newDirectTLSClient()
	if err != nil {
		return nil, nil, err
	}

	filename := "frame_" + time.Now().UTC().Format("20060102_150405") + ".png"
	previewUploadID, _, err := c.uploadFile(ctx, directClient, directClient, token, teamID, filename, "DATASET_PREVIEW", frame)
	if err != nil {
		return nil, nil, err
	}
	datasetUploadID, _, err := c.uploadFile(ctx, directClient, directClient, token, teamID, filename, "DATASET", frame)
	if err != nil {
		return nil, nil, err
	}

	assetID, imageURL, err := c.createDataset(ctx, directClient, token, teamID, filename, datasetUploadID, previewUploadID, cfg.Width, cfg.Height)
	if err != nil {
		return nil, nil, err
	}
	// Browser order: real session first, attach the first-frame asset + its own
	// asset group, THEN submit the task into it (see createSession).
	sessionID, err := c.createSession(ctx, directClient, token, teamID)
	if err != nil {
		return nil, nil, err
	}
	c.attachReference(ctx, directClient, token, teamID, sessionID, assetID)
	assetGroupID, _ := c.sessionAssetGroup(ctx, directClient, token, teamID, sessionID) // best-effort

	taskID, err := c.createTask(ctx, submitClient, directClient, token, teamID, prompt, imageURL, assetID, assetGroupID, sessionID, aspectRatio, seconds)
	if err != nil {
		return nil, nil, err
	}

	artifactURL, err := c.pollTask(ctx, directClient, token, teamID, taskID)
	if err != nil {
		return nil, nil, err
	}
	meta := map[string]any{
		"provider":  "runway",
		"task_id":   taskID,
		"team_id":   teamID,
		"video_url": artifactURL,
	}
	if !downloadResult {
		return nil, meta, nil
	}
	data, err := c.download(ctx, directClient, artifactURL)
	if err != nil {
		return nil, nil, err
	}
	return data, meta, nil
}

// uploadFile mirrors gen_video.upload_file: register the upload, PUT the bytes to
// the returned S3 URL, then complete. Returns the upload id and final url.
func (c *Client) uploadFile(ctx context.Context, controlClient, dataClient tlsclient.HttpClient, token, teamID, filename, uploadType string, data []byte) (string, string, error) {
	info, err := c.apiJSON(ctx, controlClient, token, teamID, http.MethodPost, "/v1/uploads", map[string]any{
		"filename":      filename,
		"numberOfParts": 1,
		"type":          uploadType,
	})
	if err != nil {
		return "", "", err
	}
	uploadID := strings.TrimSpace(stringValue(info["id"]))
	urls, _ := info["uploadUrls"].([]any)
	if uploadID == "" || len(urls) == 0 {
		return "", "", fmt.Errorf("%w: upload register missing fields", ErrTemporaryUpstream)
	}
	putURL := strings.TrimSpace(stringValue(urls[0]))
	contentType := "application/octet-stream"
	if hdrs, ok := info["uploadHeaders"].(map[string]any); ok {
		if ct := strings.TrimSpace(stringValue(hdrs["Content-Type"])); ct != "" {
			contentType = ct
		}
	}

	etag, err := c.putBytes(ctx, dataClient, putURL, contentType, data)
	if err != nil {
		return "", "", err
	}

	res, err := c.apiJSON(ctx, controlClient, token, teamID, http.MethodPost, "/v1/uploads/"+uploadID+"/complete", map[string]any{
		"parts": []map[string]any{{"PartNumber": 1, "ETag": etag}},
	})
	if err != nil {
		return "", "", err
	}
	return uploadID, strings.TrimSpace(stringValue(res["url"])), nil
}

func (c *Client) createDataset(ctx context.Context, client tlsclient.HttpClient, token, teamID, filename, datasetUploadID, previewUploadID string, w, h int) (string, string, error) {
	teamIDNum := jsonNumberOrString(teamID)
	res, err := c.apiJSON(ctx, client, token, teamID, http.MethodPost, "/v1/datasets", map[string]any{
		"fileCount":        1,
		"name":             filename,
		"uploadId":         datasetUploadID,
		"previewUploadIds": []string{previewUploadID},
		"metadata":         map[string]any{"size": map[string]any{"width": w, "height": h}},
		"type":             map[string]any{"name": "image", "type": "image", "isDirectory": false},
		"asTeamId":         teamIDNum,
		"privateInTeam":    true,
	})
	if err != nil {
		return "", "", err
	}
	ds, _ := res["dataset"].(map[string]any)
	id := strings.TrimSpace(stringValue(ds["id"]))
	url := strings.TrimSpace(stringValue(ds["url"]))
	if id == "" || url == "" {
		return "", "", fmt.Errorf("%w: dataset missing fields", ErrTemporaryUpstream)
	}
	return id, url, nil
}

func (c *Client) createTask(ctx context.Context, submitClient, controlClient tlsclient.HttpClient, token, teamID, prompt, imageURL, assetID, assetGroupID, sessionID, aspectRatio string, seconds int) (string, error) {
	w, h := ratioDimensions(aspectRatio)
	opts := map[string]any{
		"route":          "i2v",
		"name":           "Gen-4 Turbo - " + prompt,
		"text_prompt":    prompt,
		"seconds":        seconds,
		"width":          w,
		"height":         h,
		"init_image":     imageURL,
		"imageAssetId":   assetID,
		"exploreMode":    false,
		"creationSource": "tool-mode",
		"seed":           mrand.IntN(999999999) + 1,
		"watermark":      true,
	}
	if assetGroupID != "" {
		opts["assetGroupId"] = assetGroupID
	}
	res, err := c.submitTask(ctx, submitClient, token, teamID, map[string]any{
		"taskType":  "gen4_turbo",
		"options":   opts,
		"asTeamId":  jsonNumberOrString(teamID),
		"sessionId": sessionID,
	})
	if err != nil {
		return "", err
	}
	task, _ := res["task"].(map[string]any)
	id := strings.TrimSpace(stringValue(task["id"]))
	if id == "" {
		return "", fmt.Errorf("%w: task missing id", ErrTemporaryUpstream)
	}
	// Post-submit: generation record + session play (session already exists).
	c.recordGeneration(ctx, controlClient, token, teamID, id, sessionID, prompt, opts)
	return id, nil
}

func (c *Client) pollTask(ctx context.Context, client tlsclient.HttpClient, token, teamID, taskID string) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		res, err := c.apiJSON(ctx, client, token, teamID, http.MethodGet, "/v1/tasks/"+taskID+"?asTeamId="+teamID, nil)
		if err != nil {
			// A transient blip shouldn't kill a render that may still succeed.
			if errors.Is(err, ErrTemporaryUpstream) {
				if sleepCtx(ctx, 5*time.Second) != nil {
					return "", ctx.Err()
				}
				continue
			}
			return "", err
		}
		task, _ := res["task"].(map[string]any)
		status := strings.ToUpper(strings.TrimSpace(stringValue(task["status"])))
		switch status {
		case "SUCCEEDED":
			arts, _ := task["artifacts"].([]any)
			for _, raw := range arts {
				art, _ := raw.(map[string]any)
				if url := strings.TrimSpace(stringValue(art["url"])); url != "" {
					return url, nil
				}
			}
			return "", errors.New("runway: task succeeded with no artifact url")
		case "FAILED", "CANCELED":
			reason := strings.TrimSpace(stringValue(task["error"]))
			if isCreditError(reason) {
				return "", fmt.Errorf("%w: %s", ErrQuotaExhausted, reason)
			}
			return "", fmt.Errorf("runway: task %s: %s", status, reason)
		}
		if sleepCtx(ctx, 5*time.Second) != nil {
			return "", ctx.Err()
		}
	}
}

// estimateCost fires the /v1/billing/estimate_feature_cost_credits pre-flight the
// web app ALWAYS does before a spend (52 times in the reference HAR, always with
// the exact taskOptions right before /v1/tasks). It returns no reservation token,
// so its purpose is purely to mark the spend as coming from the real UI flow;
// submitting a task with no preceding estimate for that spec is a scripted-abuse
// tell. Best-effort — the estimate itself costs nothing.
func (c *Client) estimateCost(ctx context.Context, client tlsclient.HttpClient, token, teamID, feature string, taskOptions map[string]any) {
	_, _ = c.apiJSON(ctx, client, token, teamID, http.MethodPost, "/v1/billing/estimate_feature_cost_credits", map[string]any{
		"feature":     feature,
		"count":       1,
		"asTeamId":    jsonNumberOrString(teamID),
		"taskOptions": taskOptions,
	})
}

// createSession creates an EMPTY tool-mode session (POST /v1/sessions with an
// empty taskIds array) and returns its real server-side id. This is the crux of
// the whole flow: the browser creates the session BEFORE submitting the first
// task, then submits the task INTO that real session. Submitting /v1/tasks with a
// sessionId that was never created server-side is a forgery tell that makes
// Runway revoke the account's free credits (permitted numPlanCredits 500 -> 0)
// the instant the task lands ("提交生图就清零"). Note taskIds must be [] — passing
// a bogus id yields "Invalid task IDs".
func (c *Client) createSession(ctx context.Context, client tlsclient.HttpClient, token, teamID string) (string, error) {
	res, err := c.apiJSON(ctx, client, token, teamID, http.MethodPost, "/v1/sessions", map[string]any{
		"asTeamId": jsonNumberOrString(teamID),
		"taskIds":  []string{},
	})
	if err != nil {
		return "", err
	}
	sess, _ := res["session"].(map[string]any)
	id := strings.TrimSpace(stringValue(sess["id"]))
	if id == "" {
		return "", fmt.Errorf("%w: session missing id", ErrTemporaryUpstream)
	}
	return id, nil
}

// attachReference attaches an uploaded asset to the session (the browser does
// this before the task, so the task's reference_images belong to a real session).
func (c *Client) attachReference(ctx context.Context, client tlsclient.HttpClient, token, teamID, sessionID, assetID string) {
	if strings.TrimSpace(assetID) == "" {
		return
	}
	_, _ = c.apiJSON(ctx, client, token, teamID, http.MethodPost,
		"/v1/sessions/"+sessionID+"/references", map[string]any{
			"assetId":  assetID,
			"asTeamId": jsonNumberOrString(teamID),
		})
}

// sessionAssetGroup creates the session's OWN asset group (POST
// /v1/sessions/{id}/assetGroup) and returns its id — this, NOT the account-wide
// "Generations" group, is the assetGroupId the browser puts on the task.
func (c *Client) sessionAssetGroup(ctx context.Context, client tlsclient.HttpClient, token, teamID, sessionID string) (string, error) {
	res, err := c.apiJSON(ctx, client, token, teamID, http.MethodPost,
		"/v1/sessions/"+sessionID+"/assetGroup", map[string]any{"asTeamId": jsonNumberOrString(teamID)})
	if err != nil {
		return "", err
	}
	ag, _ := res["assetGroup"].(map[string]any)
	return strings.TrimSpace(stringValue(ag["id"])), nil
}

// recordGeneration is the post-submit lifecycle the browser fires AFTER the task
// lands: the generation record (POST /v1/generations, recordingEnabled=true) and
// a session /play. The session itself already exists (createSession ran before
// the task), so this no longer creates it. Best-effort.
func (c *Client) recordGeneration(ctx context.Context, client tlsclient.HttpClient, token, teamID, taskID, sessionID, prompt string, taskOptions map[string]any) {
	settings := map[string]any{"taskId": taskID, "recordingEnabled": true}
	for k, v := range taskOptions {
		if k == "exploreMode" { // not present on the browser's generation record
			continue
		}
		settings[k] = v
	}
	// The generation record carries the model id WITHOUT the "-preview" suffix
	// the task uses (gemini-3-pro-image-preview -> gemini-3-pro-image in the HAR).
	if m, ok := settings["model"].(string); ok {
		settings["model"] = strings.TrimSuffix(m, "-preview")
	}
	_, _ = c.apiJSON(ctx, client, token, teamID, http.MethodPost, "/v1/generations", map[string]any{
		"toolId":   "generate",
		"prompt":   prompt,
		"outputs":  map[string]any{"outputUrls": []any{}},
		"settings": settings,
	})
	_, _ = c.apiJSON(ctx, client, token, teamID, http.MethodPost, "/v1/sessions/"+sessionID+"/play",
		map[string]any{"asTeamId": jsonNumberOrString(teamID), "taskId": taskID})
}

// submitTask POSTs a /v1/tasks create. Transient (network / 5xx) failures
// surface as ErrTemporaryUpstream so the pool fails over to the NEXT account
// (换号重试) instead of retrying this one.
func (c *Client) submitTask(ctx context.Context, client tlsclient.HttpClient, token, teamID string, body map[string]any) (map[string]any, error) {
	return c.apiJSON(ctx, client, token, teamID, http.MethodPost, "/v1/tasks", body)
}

// apiJSON performs an authed JSON request against the Runway API and returns the
// parsed body, mapping status codes to the shared provider error sentinels.
func (c *Client) apiJSON(ctx context.Context, client tlsclient.HttpClient, token, teamID, method, path string, body any) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, apiBase+path, reader)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header = browserHeaders(token, teamID)
	// The generate-submit endpoint is the ONE authed call the real web bundle
	// sends WITHOUT x-runway-client-id / source-application[-version] — every
	// other endpoint carries them, /v1/tasks carries only x-runway-workspace
	// (verified across the whole reference HAR). Leaving them on /v1/tasks is a
	// bot tell that gets the account's free credits revoked (permitted
	// numPlanCredits 500 -> 0) the instant a task is submitted. Strip them here so
	// the submit matches the browser exactly.
	if method == http.MethodPost && path == "/v1/tasks" {
		// NOTE: raw map delete, NOT req.Header.Del() — fhttp canonicalizes the key
		// in Del() ("X-Runway-Client-Id") but our headers are stored lowercase (so
		// they go on the h2 wire lowercase like Chrome), so Del() silently no-ops.
		delete(req.Header, "x-runway-client-id")
		delete(req.Header, "x-runway-source-application")
		delete(req.Header, "x-runway-source-application-version")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTemporaryUpstream, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == 401:
		return nil, fmt.Errorf("%w: %s %d", ErrAuth, path, resp.StatusCode)
	case resp.StatusCode == 403 && runwayDefinitiveAuth(raw):
		return nil, fmt.Errorf("%w: %s %d", ErrAuth, path, resp.StatusCode)
	case resp.StatusCode == 403:
		// A bare 403 can be edge/risk/rate-limit policy and is not evidence that
		// the durable account credential is invalid. Never destructively disable
		// an account unless the response carries a definitive token verdict.
		return nil, fmt.Errorf("%w: %s 403", ErrTemporaryUpstream, path)
	case resp.StatusCode == 429:
		return nil, fmt.Errorf("%w: %s 429 %s", ErrQuotaExhausted, path, clip(raw, 200))
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("%w: %s %d %s", ErrTemporaryUpstream, path, resp.StatusCode, clip(raw, 200))
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		if isCreditError(string(raw)) {
			return nil, fmt.Errorf("%w: %s", ErrQuotaExhausted, clip(raw, 200))
		}
		return nil, fmt.Errorf("runway: %s %d %s", path, resp.StatusCode, clip(raw, 200))
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

// putBytes uploads raw bytes to a presigned S3 URL (no auth) and returns the
// ETag, mirroring the plain requests.Session().put in gen_video.py.
func (c *Client) putBytes(ctx context.Context, client tlsclient.HttpClient, url, contentType string, data []byte) (string, error) {
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req = req.WithContext(ctx)
	req.Header = browserAssetHeaders()
	req.Header.Set("content-type", contentType)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrTemporaryUpstream, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%w: s3 put %d", ErrTemporaryUpstream, resp.StatusCode)
	}
	return strings.Trim(resp.Header.Get("ETag"), `"`), nil
}

func (c *Client) download(ctx context.Context, client tlsclient.HttpClient, url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header = browserAssetHeaders()
	req.Header.Set("accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
	req.Header.Set("sec-fetch-dest", "image")
	req.Header.Set("sec-fetch-mode", "no-cors")
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
		return nil, err
	}
	if len(data) == 0 {
		return nil, errors.New("runway: empty artifact download")
	}
	return data, nil
}

// jsonNumberOrString returns the team id as a JSON number when it's purely
// numeric (Runway's asTeamId is an integer in the reference payloads), else the
// raw string.
func jsonNumberOrString(teamID string) any {
	return json.Number(strings.TrimSpace(teamID))
}

func isCreditError(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "credit") || strings.Contains(s, "insufficient") || strings.Contains(s, "quota")
}

// sleepCtx sleeps for d or until ctx is done; returns ctx.Err() if cancelled.
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
