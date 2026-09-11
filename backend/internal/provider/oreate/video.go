package oreate

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var videoURLPattern = regexp.MustCompile(`https?://[^\s"'<>\\]+?\.mp4(?:\?[^\s"'<>\\]*)?`)

// Oreate publishes every rendered video under a CDN path derived from the
// stream's logId, so a stream that drops after the job was accepted can still
// be recovered instead of discarding a submit the account already paid for.
const (
	videoCDNBase          = "https://cdn.oreateai.com/aivideo/videodownload"
	videoRecoveryInterval = 15 * time.Second
	videoRecoveryWindow   = 30 * time.Minute
)

// errStreamIncomplete marks a stream that neither produced a result nor an
// upstream verdict — the only case where logId recovery is meaningful.
var errStreamIncomplete = errors.New("stream ended without a video URL")

// videoStream is what the SSE consumer learned before the stream ended.
type videoStream struct {
	VideoURL string
	LogID    string
	Started  bool
}

type videoRequest struct {
	ClientType  string         `json:"clientType"`
	Type        string         `json:"type"`
	ChatType    string         `json:"chatType"`
	From        string         `json:"from"`
	Messages    []videoMessage `json:"messages"`
	VideoConfig videoConfig    `json:"videoConfig"`
	JT          string         `json:"jt"`
	UA          string         `json:"ua"`
	JSEnv       string         `json:"js_env"`
	Extra       requestExtra   `json:"extra"`
}

type videoMessage struct {
	Role        string            `json:"role"`
	Content     string            `json:"content"`
	Attachments []videoAttachment `json:"attachments"`
}

type videoConfig struct {
	ModelName  string                `json:"modelName"`
	Ratio      string                `json:"ratio"`
	Resolution string                `json:"resolution"`
	Duration   int                   `json:"duration"`
	IsAudio    bool                  `json:"isAudio"`
	AIType     int                   `json:"aiType"`
	Scene      string                `json:"scene"`
	TextImage  *textOrImageConfig    `json:"textOrImage,omitempty"`
	FrameBased *frameBasedConfig     `json:"frameBased,omitempty"`
	Reference  *referenceSceneConfig `json:"reference,omitempty"`
}

type textOrImageConfig struct {
	Image string `json:"image"`
}

type frameBasedConfig struct {
	FirstFrame string `json:"firstFrame"`
	LastFrame  string `json:"lastFrame"`
}

type referenceSceneConfig struct {
	ReferenceImages   []string `json:"referenceImages"`
	ReferenceVideos   []string `json:"referenceVideos"`
	RefDuration       string   `json:"refDuration"`
	RefTotalDuration  int      `json:"refTotalDuration"`
	KeepOriginalSound bool     `json:"keepOriginalSound"`
}

type videoAttachment struct {
	BOSURL           string  `json:"bos_url"`
	DocID            string  `json:"docId"`
	DocTitle         string  `json:"doc_title"`
	DocType          string  `json:"doc_type"`
	Size             int     `json:"size"`
	BOSURLAlias      string  `json:"bosUrl"`
	Flag             string  `json:"flag"`
	Type             string  `json:"type"`
	Status           int     `json:"status"`
	VideoDurationSec float64 `json:"videoDurationSec,omitempty"`
}

type VideoOptions struct {
	ModelID         string
	Prompt          string
	Ratio           string
	Resolution      string
	Duration        int
	Audio           bool
	DownloadResult  bool
	ReferenceImages []MediaReference
	ReferenceVideos []MediaReference
}

type requestExtra struct {
	DocName    string `json:"doc_name"`
	ModuleName string `json:"module_name"`
	Email      string `json:"email"`
	VIP        string `json:"vip"`
	RegTS      int64  `json:"reg_ts"`
	DeviceID   string `json:"deviceID"`
	BID        string `json:"bid"`
}

func (c *Client) GenerateVideo(ctx context.Context, account Account, options VideoOptions) ([]byte, map[string]any, error) {
	account = account.normalized()
	if account.Cookie == "" {
		return nil, nil, ErrAuth
	}
	if strings.TrimSpace(options.Prompt) == "" {
		return nil, nil, errors.New("oreate: prompt required")
	}
	if !validRatio(options.Ratio) {
		return nil, nil, errors.New("oreate: unsupported aspect ratio")
	}
	modelID := normalizeModelID(options.ModelID)
	if len(options.ReferenceImages) > 9 || len(options.ReferenceVideos) > 3 || len(options.ReferenceImages)+len(options.ReferenceVideos) > 12 {
		return nil, nil, errors.New("oreate: too many reference media items")
	}
	if modelID == "seedance-1.5-pro" {
		if len(options.ReferenceImages) > 2 {
			return nil, nil, errors.New("oreate: Seedance 1.5 Pro accepts at most two reference images")
		}
		if len(options.ReferenceVideos) > 0 {
			return nil, nil, errors.New("oreate: Seedance 1.5 Pro does not support reference videos")
		}
	} else if len(options.ReferenceImages)+len(options.ReferenceVideos) > 0 {
		if _, ok := seedanceReferenceModels[modelID]; !ok {
			return nil, nil, fmt.Errorf("oreate: model %q does not support reference media", options.ModelID)
		}
	}

	modelName, aiType, err := SeedanceConfig(modelID, options.Resolution, options.Duration, options.Audio)
	if err != nil {
		return nil, nil, err
	}
	refDuration, durationBand := 0, ""
	if len(options.ReferenceVideos) > 0 {
		totalDuration := 0.0
		for i := range options.ReferenceVideos {
			if options.ReferenceVideos[i].DurationSec <= 0 {
				options.ReferenceVideos[i].DurationSec, err = MP4DurationSeconds(options.ReferenceVideos[i].Data)
				if err != nil {
					return nil, nil, err
				}
			}
			totalDuration += options.ReferenceVideos[i].DurationSec
		}
		refDuration = int(math.Ceil(totalDuration))
		modelName, aiType, durationBand, err = SeedanceReferenceConfig(modelID, options.Resolution, options.Duration, refDuration)
		if err != nil {
			return nil, nil, err
		}
	}
	resolution := normalizeResolution(options.Resolution)

	uploadedImages, uploadedVideos, err := c.uploadReferences(ctx, account, options.ReferenceImages, options.ReferenceVideos)
	if err != nil {
		return nil, nil, err
	}

	if c.signer == nil {
		return nil, nil, errors.New("oreate: signer not configured")
	}
	// The page mints the token and posts the stream itself, so jt and bid are
	// left empty here and filled in by the page.
	submitter, ok := c.signer.(videoSubmitter)
	if !ok {
		return nil, nil, errors.New("oreate: signer cannot submit generations from a page")
	}
	config := videoConfig{
		ModelName: modelName, Ratio: options.Ratio, Resolution: resolution, Duration: options.Duration,
		IsAudio: options.Audio, AIType: aiType,
	}
	attachments := make([]videoAttachment, 0, len(uploadedImages)+len(uploadedVideos))
	imagePaths := make([]string, 0, len(uploadedImages))
	videoPaths := make([]string, 0, len(uploadedVideos))
	for _, item := range uploadedImages {
		imagePaths = append(imagePaths, item.ObjectPath)
		attachments = append(attachments, item.Attachment)
	}
	for _, item := range uploadedVideos {
		videoPaths = append(videoPaths, item.ObjectPath)
		attachments = append(attachments, item.Attachment)
	}
	switch {
	case modelID == "seedance-1.5-pro" && len(imagePaths) == 2:
		config.Scene = "frame_based"
		config.FrameBased = &frameBasedConfig{FirstFrame: imagePaths[0], LastFrame: imagePaths[1]}
	case modelID == "seedance-1.5-pro" && len(imagePaths) == 1:
		config.Scene = "text_or_image"
		config.TextImage = &textOrImageConfig{Image: imagePaths[0]}
	case len(imagePaths)+len(videoPaths) > 0:
		config.Scene = "reference"
		config.Reference = &referenceSceneConfig{
			ReferenceImages: imagePaths, ReferenceVideos: videoPaths, RefDuration: durationBand,
			RefTotalDuration: refDuration, KeepOriginalSound: false,
		}
	default:
		config.Scene = "text_or_image"
		config.TextImage = &textOrImageConfig{Image: ""}
	}
	payload := videoRequest{
		ClientType: "pc", Type: "chat",
		ChatType: "aiVideo", From: "home",
		Messages:    []videoMessage{{Role: "user", Content: options.Prompt, Attachments: attachments}},
		VideoConfig: config,
		UA:          account.UserAgent, JSEnv: "h5",
		Extra: requestExtra{
			DocName: "", ModuleName: "gpt4o", Email: account.Email, VIP: account.VIP,
			RegTS: account.RegTS, DeviceID: account.OUID, BID: account.BID,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	result, err := submitter.SubmitVideo(ctx, account, body)
	if err != nil {
		log.Printf("oreate video submit failed: %s", clipSignerLog(err))
		return nil, nil, err
	}
	if IsOreateCookie(result.Cookie) {
		account.Cookie = result.Cookie
		account.OUID = cookieValue(result.Cookie, "OUID")
		account.BID = cookieValue(result.Cookie, "__bid_n")
	}
	submitFailure := strings.TrimSpace(result.Failure)
	status, err := inPageStreamStatus(result)
	if err != nil {
		return nil, nil, err
	}
	// A dropped stream still leaves the events the page already read, which is
	// what logId recovery below needs.
	streamBody := io.NopCloser(strings.NewReader(result.Stream))
	defer streamBody.Close()
	if status != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(streamBody, 1024))
		return nil, nil, classifyUpstreamError(status, string(body))
	}
	stream, err := parseVideoSSE(streamBody)
	videoURL := stream.VideoURL
	if err != nil {
		// A logId means the submit was accepted upstream, so the render can still
		// land on the CDN even when the stream never reported a start event.
		if !errors.Is(err, errStreamIncomplete) {
			return nil, nil, err
		}
		if stream.LogID == "" {
			return nil, nil, err
		}
		recovered := c.awaitVideoByLogID(ctx, stream.LogID)
		if recovered == "" {
			if submitFailure != "" {
				return nil, nil, fmt.Errorf("%w (log_id %s, browser: %s)", err, stream.LogID, submitFailure)
			}
			return nil, nil, fmt.Errorf("%w (log_id %s)", err, stream.LogID)
		}
		videoURL = recovered
	}
	meta := map[string]any{"provider": "oreate", "video_url": videoURL, "log_id": stream.LogID}
	if !options.DownloadResult {
		return nil, meta, nil
	}
	data, err := c.downloadVideo(ctx, videoURL)
	if err != nil {
		return nil, nil, err
	}
	return data, meta, nil
}

// awaitVideoByLogID polls the logId path until the rendered file shows up, and
// returns an empty string once the bounded window elapses.
func (c *Client) awaitVideoByLogID(ctx context.Context, logID string) string {
	rawURL := c.videoCDNURL(logID)
	deadline := time.Now().Add(videoRecoveryWindow)
	for {
		if c.videoReady(ctx, rawURL) {
			return rawURL
		}
		if !time.Now().Before(deadline) {
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(videoRecoveryInterval):
		}
	}
}

func (c *Client) videoReady(ctx context.Context, rawURL string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return false
	}
	resp, err := c.httpClient(false).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	return resp.StatusCode == http.StatusOK
}

func (c *Client) videoCDNURL(logID string) string {
	base := strings.TrimRight(strings.TrimSpace(c.cdnBaseURL), "/")
	if base == "" {
		base = videoCDNBase
	}
	return base + "/" + strings.TrimSpace(logID) + ".mp4"
}

// newStreamScanner reads one SSE line at a time; generation streams carry
// message payloads far beyond the default scanner limit.
func newStreamScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	return scanner
}

func parseVideoSSE(r io.Reader) (videoStream, error) {
	scanner := newStreamScanner(r)
	stream := videoStream{}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			if found := extractVideoURL(payload); found != "" {
				stream.VideoURL = found
			}
			continue
		}
		if logID := scalarString(event["logId"], ""); logID != "" {
			stream.LogID = logID
		}
		switch strings.ToLower(scalarString(event["event"], "")) {
		case "start", "generating":
			stream.Started = true
		}
		if found := findVideoURL(event); found != "" {
			stream.VideoURL = found
		}
		if strings.EqualFold(scalarString(event["event"], ""), "error") {
			data, _ := event["data"].(map[string]any)
			code := intFromAny(data["code"])
			msg := scalarString(data["msg"], scalarString(data["message"], "upstream error"))
			return stream, classifyUpstreamError(code, msg)
		}
	}
	if err := scanner.Err(); err != nil {
		return stream, fmt.Errorf("%w: %w: read stream: %v", ErrTemporaryUpstream, errStreamIncomplete, err)
	}
	if stream.VideoURL == "" {
		return stream, fmt.Errorf("%w: %w", ErrTemporaryUpstream, errStreamIncomplete)
	}
	return stream, nil
}

func findVideoURL(v any) string {
	switch x := v.(type) {
	case string:
		if found := extractVideoURL(x); found != "" {
			return found
		}
		var nested any
		if json.Unmarshal([]byte(x), &nested) == nil {
			return findVideoURL(nested)
		}
	case map[string]any:
		for _, key := range []string{"video_url", "videoUrl", "url", "content", "result", "data"} {
			if found := findVideoURL(x[key]); found != "" {
				return found
			}
		}
		for _, value := range x {
			if found := findVideoURL(value); found != "" {
				return found
			}
		}
	case []any:
		for _, value := range x {
			if found := findVideoURL(value); found != "" {
				return found
			}
		}
	}
	return ""
}

func extractVideoURL(s string) string {
	s = strings.ReplaceAll(s, `\/`, "/")
	return videoURLPattern.FindString(s)
}

func classifyUpstreamError(code int, message string) error {
	msg := strings.TrimSpace(message)
	lower := strings.ToLower(msg)
	log.Printf("oreate upstream verdict: code=%d", code)
	switch {
	case code == 7350001 || strings.Contains(msg, "请输入验证码") || strings.Contains(lower, "captcha") || strings.Contains(lower, "verification code"):
		return fmt.Errorf("%w: %w: %s", ErrAccountChallenge, ErrRiskControl, msg)
	case code == 212361 || strings.Contains(lower, "spam user") || strings.Contains(lower, "risk control"):
		if code == 212361 || strings.Contains(lower, "spam user") {
			// An explicit spam-user response is an account-level risk decision.
			// Keep it distinguishable from transient Banti risk-control failures so
			// the scheduler can remove the affected account from rotation.
			return fmt.Errorf("%w: %w: %s", ErrSpamUser, ErrRiskControl, msg)
		}
		return fmt.Errorf("%w: %s", ErrRiskControl, msg)
	case code == 200017 || strings.Contains(lower, "point exceed") || strings.Contains(lower, "insufficient") || strings.Contains(lower, "not enough point"):
		return fmt.Errorf("%w: %s", ErrQuotaExhausted, msg)
	case code == http.StatusUnauthorized || code == http.StatusForbidden || strings.Contains(lower, "unauth") || strings.Contains(lower, "login") || strings.Contains(lower, "cookie") && strings.Contains(lower, "invalid"):
		return fmt.Errorf("%w: %s", ErrAuth, msg)
	case strings.Contains(lower, "safety") || strings.Contains(lower, "sensitive") || strings.Contains(lower, "content policy"):
		return fmt.Errorf("%w: %s", ErrContentRejected, msg)
	case code >= 500 || code == http.StatusTooManyRequests || strings.Contains(lower, "busy") || strings.Contains(lower, "timeout"):
		return fmt.Errorf("%w: %s", ErrTemporaryUpstream, msg)
	default:
		return fmt.Errorf("oreate upstream error %d: %s", code, msg)
	}
}

func intFromAny(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	case string:
		var n int
		_, _ = fmt.Sscanf(strings.TrimSpace(x), "%d", &n)
		return n
	default:
		return 0
	}
}

func (c *Client) downloadVideo(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "video/mp4,video/*;q=0.9,*/*;q=0.8")
	resp, err := c.httpClient(false).Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: video download: %v", ErrTemporaryUpstream, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: video download http %d", ErrTemporaryUpstream, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
