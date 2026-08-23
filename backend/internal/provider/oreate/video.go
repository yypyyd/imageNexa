package oreate

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
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
	messageListPath       = "/oreate/memory/getmessagelist"
	chatVideoPollInterval = 10 * time.Second
	chatVideoMaxFailures  = 3
	videoRecoveryInterval = 15 * time.Second
	videoRecoveryWindow   = 15 * time.Minute
)

// errStreamIncomplete marks a stream that neither produced a result nor an
// upstream verdict — the only case where logId recovery is meaningful.
var errStreamIncomplete = errors.New("stream ended without a video URL")

var htmlTagPattern = regexp.MustCompile(`<[^>]*>`)

// errChatUnavailable marks a chat that cannot be read at all, as opposed to one
// that has not reported a result yet.
var errChatUnavailable = errors.New("chat history unavailable")

// chatVideoVerdict is the chat's terminal state for one generation: Oreate keeps
// the render result on the assistant message, which is authoritative even when
// the event stream dropped or only ever carried pings.
type chatVideoVerdict struct {
	VideoURL string
	Failed   bool
	Pending  bool
	Message  string
}

// videoStream is what the SSE consumer learned before the stream ended.
type videoStream struct {
	VideoURL string
	LogID    string
	Started  bool
}

type videoRequest struct {
	ClientType  string         `json:"clientType"`
	Type        string         `json:"type"`
	FocusID     string         `json:"focusId"`
	ChatID      string         `json:"chatId"`
	ChatType    string         `json:"chatType"`
	From        string         `json:"from"`
	ChatTitle   string         `json:"chatTitle"`
	IsFirst     bool           `json:"isFirst"`
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
	// The page mints the token, opens the chat and posts the request itself, so
	// the identity fields are left empty here and filled in by the page.
	submitter, ok := c.signer.(videoSubmitter)
	if !ok {
		return nil, nil, errors.New("oreate: signer cannot submit generations from a page")
	}
	var chatID, egress string
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
		ClientType: "pc", Type: "chat", FocusID: chatID, ChatID: chatID,
		ChatType: "aiVideo", From: "home", ChatTitle: "Unnamed Session", IsFirst: true,
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
		return nil, nil, err
	}
	chatID = result.ChatID
	// The chat is polled from Go, so it has to leave through the session the
	// submitting page used: upstream sees one exit IP per generation.
	egress = result.Proxy
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
		// The page hands the render over as soon as upstream accepts it, so the
		// chat is where the result arrives: it reports the finished clip or the
		// upstream giving up, and only an unreadable chat falls back to the CDN.
		verdict := c.awaitChatVideo(ctx, account, chatID, egress)
		switch {
		case verdict.VideoURL != "":
			videoURL = verdict.VideoURL
		case verdict.Failed:
			return nil, nil, fmt.Errorf("%w: upstream reported render failure: %s", ErrTemporaryUpstream, upstreamFailureMessage(verdict.Message))
		case stream.LogID == "":
			return nil, nil, err
		default:
			recovered := c.awaitVideoByLogID(ctx, stream.LogID)
			if recovered == "" {
				if submitFailure != "" {
					return nil, nil, fmt.Errorf("%w (log_id %s, browser: %s)", err, stream.LogID, submitFailure)
				}
				return nil, nil, fmt.Errorf("%w (log_id %s)", err, stream.LogID)
			}
			videoURL = recovered
		}
	}
	meta := map[string]any{"provider": "oreate", "chat_id": chatID, "video_url": videoURL, "log_id": stream.LogID}
	if !options.DownloadResult {
		return nil, meta, nil
	}
	data, err := c.downloadVideo(ctx, videoURL)
	if err != nil {
		return nil, nil, err
	}
	return data, meta, nil
}

// createChat opens a conversation through the given proxy session, which for a
// video generation is the session that minted the token the submit will carry.
func (c *Client) createChat(ctx context.Context, account Account, chatType, proxyURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/oreate/create/chat"), strings.NewReader(`{"type":"`+chatType+`"}`))
	if err != nil {
		return "", err
	}
	setHeaders(req, account, "application/json")
	resp, err := c.egressClient(proxyURL).Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: create chat: %v", ErrTemporaryUpstream, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", ErrAuth
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", err
	}
	return parseCreateChat(body)
}

func parseCreateChat(body []byte) (string, error) {
	var env statusEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return "", fmt.Errorf("%w: invalid create-chat response", ErrTemporaryUpstream)
	}
	if env.Status.Code != 0 {
		return "", classifyUpstreamError(env.Status.Code, env.Status.Msg)
	}
	var data struct {
		ChatID string `json:"chatId"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil || strings.TrimSpace(data.ChatID) == "" {
		return "", fmt.Errorf("%w: create-chat response missing chatId", ErrTemporaryUpstream)
	}
	return strings.TrimSpace(data.ChatID), nil
}

// awaitChatVideo reads the conversation back until it reports a terminal state,
// and returns an empty verdict once the bounded window elapses. Transient read
// failures are retried because this poll is the only thing still watching the
// render, but a chat that answers with an error status is not worth the window.
func (c *Client) awaitChatVideo(ctx context.Context, account Account, chatID, proxyURL string) chatVideoVerdict {
	if strings.TrimSpace(chatID) == "" {
		return chatVideoVerdict{}
	}
	deadline := time.Now().Add(videoRecoveryWindow)
	failures := 0
	for {
		verdict, err := c.chatVideo(ctx, account, chatID, proxyURL)
		switch {
		case errors.Is(err, errChatUnavailable):
			return chatVideoVerdict{}
		case err != nil:
			failures++
			if failures > chatVideoMaxFailures {
				return chatVideoVerdict{}
			}
		default:
			failures = 0
			if !verdict.Pending {
				return verdict
			}
		}
		if !time.Now().Before(deadline) {
			return chatVideoVerdict{}
		}
		select {
		case <-ctx.Done():
			return chatVideoVerdict{}
		case <-time.After(chatVideoPollInterval):
		}
	}
}

func (c *Client) chatVideo(ctx context.Context, account Account, chatID, proxyURL string) (chatVideoVerdict, error) {
	query := url.Values{"chatID": {chatID}, "pn": {"1"}, "rn": {"20"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint(messageListPath)+"?"+query.Encode(), nil)
	if err != nil {
		return chatVideoVerdict{}, err
	}
	setHeaders(req, account, "application/json")
	resp, err := c.egressClient(proxyURL).Do(req)
	if err != nil {
		return chatVideoVerdict{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return chatVideoVerdict{}, fmt.Errorf("%w: http %d", errChatUnavailable, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return chatVideoVerdict{}, err
	}
	return parseChatVideo(body)
}

// parseChatVideo reads the assistant message state: 3 carries the rendered clip,
// 2 is upstream giving up, anything else means the render is still running.
func parseChatVideo(body []byte) (chatVideoVerdict, error) {
	var env statusEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return chatVideoVerdict{}, fmt.Errorf("%w: invalid message-list response", ErrTemporaryUpstream)
	}
	if env.Status.Code != 0 {
		return chatVideoVerdict{}, classifyUpstreamError(env.Status.Code, env.Status.Msg)
	}
	var data struct {
		MessageList []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
			Data    string `json:"data"`
		} `json:"messageList"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return chatVideoVerdict{}, fmt.Errorf("%w: invalid message list", ErrTemporaryUpstream)
	}
	for _, message := range data.MessageList {
		if !strings.EqualFold(message.Role, "assistant") {
			continue
		}
		var state struct {
			Status int `json:"status"`
		}
		if message.Data != "" {
			_ = json.Unmarshal([]byte(message.Data), &state)
		}
		switch state.Status {
		case 3:
			if found := extractVideoURL(message.Content); found != "" {
				return chatVideoVerdict{VideoURL: found}, nil
			}
		case 2:
			return chatVideoVerdict{Failed: true, Message: message.Content}, nil
		default:
			return chatVideoVerdict{Pending: true}, nil
		}
	}
	// The assistant row shows up moments after the submit, so a chat without one
	// is still an unfinished render rather than a lost one.
	return chatVideoVerdict{Pending: true}, nil
}

// upstreamFailureMessage turns the chat message, which is rendered HTML, into a
// single readable line for the error the caller reports.
func upstreamFailureMessage(message string) string {
	cleaned := strings.Join(strings.Fields(htmlTagPattern.ReplaceAllString(message, " ")), " ")
	if cleaned == "" {
		return "upstream gave no reason"
	}
	if len(cleaned) > 200 {
		cleaned = strings.TrimSpace(cleaned[:200])
	}
	return cleaned
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
	switch {
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
