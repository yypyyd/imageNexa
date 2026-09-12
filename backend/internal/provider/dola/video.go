package dola

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	videoPromptPrefix = "生成影片："
	videoPollInterval = 5 * time.Second
	videoPollWindow   = 30 * time.Minute
	videoSubmitWait   = 3 * time.Minute
	chainPollWait     = 30 * time.Second
	completionPath    = "/chat/completion"
	chainPath         = "/im/chain/single"
	maxSSEBytes       = 8 << 20
)

// An explicit duration refusal is a terminal request error, not a transient
// account failure. It must not fan out to other accounts or charge a generation.
var ErrVideoNotStarted = errors.New("dola: upstream declined 30-second video before generation")
var ErrCopyrightRejected = fmt.Errorf("%w: copyright protection", ErrContentRejected)
var ErrAudioCopyrightRejected = fmt.Errorf("%w: audio copyright protection", ErrCopyrightRejected)
var durationRefusalPattern = regexp.MustCompile(`(?i)video generation currently supports durations|视频生成.{0,12}支持.{0,12}(4|5).{0,12}15`)

// The legacy balance endpoint does not currently return a usable balance for
// the tested accounts. Only an explicit upstream exhaustion verdict seals a day.
var creditFailPattern = regexp.MustCompile(
	`(?i)` +
		`(今日|今天|每日|每天).{0,20}(上限|用完|次数不足|次數不足)|` +
		`余额不足|餘額不足|额度不足|額度不足|额度耗尽|額度耗盡|` +
		`残高不足|` +
		`한도.*부족|부족.*한도|` +
		`insufficient (credits|quota|balance)|reached.{0,20}daily limit|daily.{0,20}limit.{0,20}(reached|exceeded)|no.{0,10}(generations|credits).{0,10}(left|remaining)`)

type MediaReference struct {
	Data        []byte
	ContentType string
	Filename    string
}

type VideoOptions struct {
	ModelID string
	Prompt  string
	Ratio   string
	// Duration must be exactly 30 seconds.
	Duration int
	// ReferenceImages turn the request into image-to-video. Each image is
	// uploaded through the ImageX chain and attached to the message. The
	// public Seedance 2.5 route accepts up to ten.
	ReferenceImages []MediaReference
}

// GenerateVideo submits one text-to-video or image-to-video request and
// polls the conversation until Dola publishes the clip. It returns the
// artifact URL in meta; callers own the download so bytes flow through the
// shared netguard pipeline.
func (c *Client) GenerateVideo(ctx context.Context, account Account, options VideoOptions) ([]byte, map[string]any, error) {
	account = account.normalized()
	if account.Cookie == "" {
		return nil, nil, ErrAuth
	}
	if strings.TrimSpace(options.Prompt) == "" {
		return nil, nil, errors.New("dola: prompt required")
	}
	if !validRatio(options.Ratio) {
		return nil, nil, errors.New("dola: unsupported aspect ratio")
	}
	if _, err := RequiredCredits(options.Duration); err != nil {
		return nil, nil, err
	}
	duration := options.Duration
	model := NormalizeModelID(options.ModelID)

	if c.usesProtocol() {
		prepared, prepErr := c.prepareProtocolAccount(ctx, account)
		if prepErr != nil {
			return nil, nil, prepErr
		}
		account = prepared
	}

	imageURIs := make([]string, 0, len(options.ReferenceImages))
	for _, ref := range options.ReferenceImages {
		uri, err := c.UploadImage(ctx, account, ref.Data, ref.ContentType)
		if err != nil {
			return nil, nil, err
		}
		imageURIs = append(imageURIs, uri)
	}

	webTabID := uuid.NewString()
	conversationID, pollingAccount, err := c.submitVideo(ctx, account, model, options.Prompt, options.Ratio, duration, imageURIs, webTabID)
	if err != nil {
		return nil, nil, err
	}
	videoURL, err := c.pollVideo(ctx, pollingAccount, conversationID, webTabID)
	if err != nil {
		if !errors.Is(err, ErrQuotaExhausted) && !errors.Is(err, ErrVideoNotStarted) {
			err = AcceptedFailure(conversationID, err)
		}
		return nil, nil, err
	}
	meta := map[string]any{
		"provider":        "dola",
		"conversation_id": conversationID,
		"video_url":       videoURL,
	}
	return nil, meta, nil
}

// submitVideo posts the chat completion and returns the conversation the
// server acknowledged. The prefix is load-bearing: without it the request is
// downgraded to image generation. The submit stream closes after the assistant
// acknowledges; a bounded window stops a stuck stream from holding the slot.
func (c *Client) submitVideo(ctx context.Context, account Account, model, prompt, ratio string, duration int, imageURIs []string, webTabID string) (string, Account, error) {
	ctx, cancel := context.WithTimeout(ctx, videoSubmitWait)
	defer cancel()
	body := buildVideoBody(model, prompt, ratio, duration, account.FP, imageURIs)
	query := commonQueryForTab(account, webTabID)
	var data []byte
	var status int
	var err error
	pollingAccount := account
	if c.usesProtocol() {
		if len(account.ProtocolQuery) == 0 {
			pollingAccount, err = c.prepareProtocolAccount(ctx, account)
			if err != nil {
				return "", account, err
			}
		}
		query = commonQueryForTab(pollingAccount, webTabID)
		if err = c.initializeProtocolVideo(ctx, pollingAccount, query); err != nil {
			return "", pollingAccount, err
		}
		data, status, err = c.protocolCompletion(ctx, pollingAccount, query, body)
	} else {
		data, status, err = c.postJSON(ctx, account, completionPath, query, body,
			"application/json", "str, str", c.endpoint("/chat/"), maxSSEBytes)
		if err != nil {
			err = fmt.Errorf("%w: %v", ErrTaskSubmissionUnknown, err)
		}
	}
	if err != nil {
		var accepted *acceptedBrowserVideo
		if errors.As(err, &accepted) {
			return accepted.conversationID, pollingAccount, nil
		}
		log.Printf("dola submit transport error: %v", err)
		return "", account, err
	}
	if status != http.StatusOK {
		log.Printf("dola submit http %d body=%s", status, truncateForLog(data, 400))
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return "", pollingAccount, ErrAuth
		}
		return "", pollingAccount, classifySubmitFailure(status, data)
	}
	events, err := parseSSE(data)
	if err != nil {
		log.Printf("dola submit invalid stream: %v body=%s", err, truncateForLog(data, 400))
		return "", pollingAccount, fmt.Errorf("%w: invalid completion stream", ErrTaskSubmissionUnknown)
	}
	conversationID := ""
	streamErrorCode := 0
	for _, event := range events {
		switch event.name {
		case "SSE_ACK":
			var ack struct {
				AckClientMeta struct {
					ConversationID string `json:"conversation_id"`
				} `json:"ack_client_meta"`
			}
			if json.Unmarshal(event.data, &ack) == nil {
				conversationID = strings.TrimSpace(ack.AckClientMeta.ConversationID)
			}
		case "STREAM_ERROR":
			var payload struct {
				ErrorCode int `json:"error_code"`
			}
			if json.Unmarshal(event.data, &payload) == nil {
				streamErrorCode = payload.ErrorCode
			}
		}
	}
	if conversationID == "" {
		log.Printf("dola submit stream error code=%d", streamErrorCode)
		return "", pollingAccount, classifySubmitFailure(status, data)
	}
	log.Printf("dola protocol accepted account=%s conversation=%s", account.ID, conversationID)
	return conversationID, pollingAccount, nil
}

func truncateForLog(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// classifySubmitFailure inspects a rejected submission for the refusal copy
// the assistant streams back, so an exhausted daily allowance fails over to
// the next account instead of waiting out the poll window.
func classifySubmitFailure(status int, body []byte) error {
	if streamErrorCode(body) == 710022004 {
		return ErrChallenge
	}
	if text := extractStreamText(body); text != "" && creditFailPattern.MatchString(text) {
		return ErrQuotaExhausted
	}
	if status >= 400 && status < 500 {
		return fmt.Errorf("%w: completion rejected (http %d)", ErrTemporaryUpstream, status)
	}
	return fmt.Errorf("%w: completion failed (http %d)", ErrTaskSubmissionUnknown, status)
}

func streamErrorCode(body []byte) int {
	events, err := parseSSE(body)
	if err != nil {
		return 0
	}
	for _, event := range events {
		if event.name != "STREAM_ERROR" {
			continue
		}
		var payload struct {
			ErrorCode int `json:"error_code"`
		}
		if json.Unmarshal(event.data, &payload) == nil {
			return payload.ErrorCode
		}
	}
	return 0
}

type sseEvent struct {
	name string
	data json.RawMessage
}

// parseSSE splits one buffered SSE body into events, tolerating the split
// multi-line data frames the transport emits.
func parseSSE(body []byte) ([]sseEvent, error) {
	var events []sseEvent
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	scanner.Buffer(make([]byte, 0, 64<<10), maxSSEBytes)
	var name string
	var dataLines []string
	flush := func() {
		if name == "" && len(dataLines) == 0 {
			return
		}
		raw := json.RawMessage(strings.Join(dataLines, "\n"))
		events = append(events, sseEvent{name: name, data: raw})
		name = ""
		dataLines = nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
	return events, scanner.Err()
}

// extractStreamText pulls readable assistant text out of a raw SSE body for
// refusal classification.
func extractStreamText(body []byte) string {
	events, err := parseSSE(body)
	if err != nil {
		return ""
	}
	var builder strings.Builder
	for _, event := range events {
		var payload struct {
			Content struct {
				ContentBlock []struct {
					Content struct {
						TextBlock struct {
							Text string `json:"text"`
						} `json:"text_block"`
					} `json:"content"`
				} `json:"content_block"`
			} `json:"content"`
			PatchOp []struct {
				PatchValue struct {
					ContentBlock []struct {
						Content struct {
							TextBlock struct {
								Text string `json:"text"`
							} `json:"text_block"`
						} `json:"content"`
					} `json:"content_block"`
				} `json:"patch_value"`
			} `json:"patch_op"`
		}
		if json.Unmarshal(event.data, &payload) != nil {
			continue
		}
		for _, block := range payload.Content.ContentBlock {
			builder.WriteString(block.Content.TextBlock.Text)
		}
		for _, op := range payload.PatchOp {
			for _, block := range op.PatchValue.ContentBlock {
				builder.WriteString(block.Content.TextBlock.Text)
			}
		}
	}
	return builder.String()
}

// pollVideo reads the conversation chain until the video creation block with
// a downloadable artifact appears, the assistant refuses, or the window ends.
func (c *Client) pollVideo(ctx context.Context, account Account, conversationID, webTabID string) (string, error) {
	deadline := time.Now().Add(videoPollWindow)
	referer := c.endpoint("/chat/" + conversationID)
	for {
		_, verdict, err := c.chainOnce(ctx, account, conversationID, referer, webTabID)
		if err == nil {
			switch {
			case verdict.videoURL != "":
				if verdict.videoDuration > 0 && (verdict.videoDuration < 29 || verdict.videoDuration > 31) {
					return "", fmt.Errorf("dola: returned video duration %.3fs, expected 30s", verdict.videoDuration)
				}
				return verdict.videoURL, nil
			case verdict.notStarted:
				return "", ErrVideoNotStarted
			case verdict.contentRefused:
				if verdict.audioCopyrightRefused {
					return "", ErrAudioCopyrightRejected
				}
				if verdict.copyrightRefused {
					return "", ErrCopyrightRejected
				}
				return "", ErrContentRejected
			case verdict.quotaRefused:
				return "", ErrQuotaExhausted
			}
		} else if !errors.Is(err, errRetryableChain) {
			return "", err
		}
		if !time.Now().Before(deadline) {
			return "", fmt.Errorf("%w: video not ready within %s", ErrTemporaryUpstream, videoPollWindow)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(videoPollInterval):
		}
	}
}

var errRetryableChain = errors.New("dola: chain poll retryable")

type acceptedBrowserVideo struct{ conversationID string }

func (e *acceptedBrowserVideo) Error() string {
	return "Dola submission recovered from durable conversation"
}

type chainVerdict struct {
	notStarted            bool
	videoSubmitted        bool
	texts                 []string
	videoURL              string
	videoDuration         float64
	quotaRefused          bool
	contentRefused        bool
	copyrightRefused      bool
	audioCopyrightRefused bool
}

// chainOnce fetches one page of the conversation chain and reduces it to a
// verdict. Transient decode failures keep polling; definitive transport
// errors abort the wait.
func (c *Client) chainOnce(ctx context.Context, account Account, conversationID, referer, webTabID string) (string, chainVerdict, error) {
	pollCtx, cancel := context.WithTimeout(ctx, chainPollWait)
	defer cancel()
	body := map[string]any{
		"cmd": 3100, "sequence_id": uuid.NewString(), "channel": 2, "version": "1",
		"uplink_body": map[string]any{"pull_singe_chain_uplink_body": map[string]any{
			"conversation_id": conversationID, "conversation_type": 3,
			"anchor_index": int64(9007199254740991), "direction": 1, "limit": 20,
			"ext": map[string]any{}, "filter": map[string]any{"index_list": []any{}},
			"evaluate_ab_params": "", "evaluate_common_params": "",
		}},
	}
	data, status, err := c.postJSON(pollCtx, account, chainPath, commonQueryForTab(account, webTabID), body,
		"application/json; encoding=utf-8", "str", referer, maxSSEBytes)
	if err != nil {
		return "", chainVerdict{}, errRetryableChain
	}
	switch {
	case status == http.StatusOK:
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "", chainVerdict{}, ErrAuth
	default:
		return "", chainVerdict{}, errRetryableChain
	}
	var envelope struct {
		Code       int    `json:"code"`
		StatusCode int    `json:"status_code"`
		Message    string `json:"message"`
		Data       struct {
			DownlinkBody struct {
				PullSingleChainDownlinkBody struct {
					Messages []chainMessage `json:"messages"`
				} `json:"pull_singe_chain_downlink_body"`
			} `json:"downlink_body"`
		} `json:"data"`
		DownlinkBody struct {
			PullSingleChainDownlinkBody struct {
				Messages []chainMessage `json:"messages"`
			} `json:"pull_singe_chain_downlink_body"`
		} `json:"downlink_body"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return "", chainVerdict{}, errRetryableChain
	}
	if envelope.StatusCode != 0 {
		envelope.Code = envelope.StatusCode
	}
	if envelope.Code != 0 {
		if isAuthCode(envelope.Code) {
			return "", chainVerdict{}, ErrAuth
		}
		return "", chainVerdict{}, errRetryableChain
	}
	messages := envelope.Data.DownlinkBody.PullSingleChainDownlinkBody.Messages
	if len(messages) == 0 {
		messages = envelope.DownlinkBody.PullSingleChainDownlinkBody.Messages
	}
	verdict := chainVerdict{}
	finalDurationRefusal := false
	for _, message := range messages {
		if message.Ext["has_video_gen"] == "1" {
			verdict.videoSubmitted = true
		}
		finalAssistant := message.UserType == 2 && message.Status != nil && *message.Status == 0
		if finalAssistant {
			switch message.Ext["ai_creation_res_code"] {
			case "710092006", "710092007":
				verdict.contentRefused = true
				verdict.copyrightRefused = true
				verdict.audioCopyrightRefused = verdict.audioCopyrightRefused || message.Ext["ai_creation_res_code"] == "710092007"
			}
		}
		if finalAssistant && message.Ext["ai_creation_res_code"] == "710082041" {
			finalDurationRefusal = true
		}
		for _, block := range message.Blocks {
			text := strings.TrimSpace(block.Content.TextBlock.Text)
			verdict.texts = append(verdict.texts, text)
			if finalAssistant && durationRefusalPattern.MatchString(text) {
				finalDurationRefusal = true
			}
			if finalAssistant && strings.Contains(strings.ToLower(text), "for copyright protection") {
				verdict.contentRefused = true
				verdict.copyrightRefused = true
				verdict.audioCopyrightRefused = verdict.audioCopyrightRefused || strings.Contains(strings.ToLower(text), "because of its audio")
			} else if text != "" && creditFailPattern.MatchString(text) {
				verdict.quotaRefused = true
			}
			if block.BlockType != 2074 {
				continue
			}
			for _, creation := range block.Content.CreationBlock.Creations {
				if creation.Type != 2 {
					continue
				}
				downloadURL, err := url.Parse(strings.TrimSpace(creation.Video.DownloadURL))
				if err != nil || downloadURL.Host == "" || downloadURL.User != nil ||
					(downloadURL.Scheme != "http" && downloadURL.Scheme != "https") {
					continue
				}
				// Dola sometimes publishes an HTTP VOD URL even though the same
				// signed path is available over TLS. Artifact delivery requires HTTPS;
				// public-address and redirect validation remain in netguard.
				downloadURL.Scheme = "https"
				verdict.videoURL = downloadURL.String()
				verdict.videoDuration = creation.Video.Duration
			}
		}
	}
	verdict.notStarted = finalDurationRefusal && !verdict.videoSubmitted && verdict.videoURL == ""
	return verdict.videoURL, verdict, nil
}

type chainMessage struct {
	UserType int               `json:"user_type"`
	Status   *int              `json:"status"`
	Ext      map[string]string `json:"ext"`
	Content  string            `json:"content"`
	Blocks   []chainBlock      `json:"content_block"`
}

func (m *chainMessage) UnmarshalJSON(data []byte) error {
	type plain chainMessage
	var raw plain
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*m = chainMessage(raw)
	if m.Content == "" {
		return nil
	}
	var blocks []chainBlock
	if err := json.Unmarshal([]byte(m.Content), &blocks); err == nil {
		m.Blocks = blocks
	}
	return nil
}

type chainBlock struct {
	BlockType int `json:"block_type"`
	Content   struct {
		TextBlock struct {
			Text string `json:"text"`
		} `json:"text_block"`
		CreationBlock struct {
			Creations []struct {
				Type  int `json:"type"`
				Video struct {
					DownloadURL string  `json:"download_url"`
					Duration    float64 `json:"duration"`
				} `json:"video"`
			} `json:"creations"`
		} `json:"creation_block"`
	} `json:"content"`
}

// buildVideoBody mirrors the webapp's chat completion payload for the video
// ability. Reference images become a leading attachment block (block_type
// 10052) before the text block, matching the browser's image-to-video shape.
func buildVideoBody(model, prompt, ratio string, duration int, fp string, imageURIs []string) map[string]any {
	now := time.Now()
	nowMS := now.UnixMilli()
	abilityParam, _ := json.Marshal(map[string]any{
		"ratio":             ratio,
		"model":             model,
		"duration":          duration,
		"input_box_content": map[string]any{"user_input_content": prompt, "reply_message_format": "Generated video: %s"},
	})

	messages := make([]map[string]any, 0, len(imageURIs)+1)
	for _, uri := range imageURIs {
		messages = append(messages, map[string]any{
			"local_message_id": uuid.NewString(),
			"content_block": []map[string]any{{
				"block_type": 10052,
				"content": map[string]any{
					"attachment_block": map[string]any{
						"attachments": []map[string]any{{
							"type":       1,
							"identifier": uuid.NewString(),
							"image": map[string]any{
								"name": "image.png",
								"uri":  uri,
								"image_ori": map[string]any{
									"url": "", "width": 1024, "height": 1024, "format": "", "url_formats": map[string]any{},
								},
							},
							"parse_state":   0,
							"review_state":  1,
							"upload_status": 1,
							"progress":      100,
							"src":           "",
						}},
					},
					"pc_event_block": "",
				},
				"block_id":      uuid.NewString(),
				"parent_id":     "",
				"meta_info":     []any{},
				"append_fields": []any{},
			}},
			"message_status": 0,
		})
	}
	messages = append(messages, map[string]any{
		"local_message_id": uuid.NewString(),
		"content_block": []map[string]any{{
			"block_type": 10000,
			"content": map[string]any{
				"text_block": map[string]any{
					"text":          "Generated video: " + prompt + ", " + ratio,
					"icon_url":      "",
					"icon_url_dark": "",
					"summary":       "",
				},
				"pc_event_block": "",
			},
			"block_id":      uuid.NewString(),
			"parent_id":     "",
			"meta_info":     []any{},
			"append_fields": []any{},
		}},
		"message_status": 0,
	})

	return map[string]any{
		"client_meta": map[string]any{
			"local_conversation_id": fmt.Sprintf("local_%d", nowMS),
			"conversation_id":       "",
			"bot_id":                dolaBotID,
			"last_section_id":       "",
			"last_message_index":    nil,
			"local_permissions": []map[string]any{
				{"permission_name": "ACCESS_COARSE_LOCATION", "status": 3},
				{"permission_name": "ACCESS_FINE_LOCATION", "status": 3},
				{"permission_name": "ACCESS_BACKGROUND_LOCATION", "status": 3},
			},
		},
		"messages": messages,
		"option": map[string]any{
			"send_message_scene":       "",
			"create_time_ms":           nowMS,
			"collect_id":               "",
			"is_audio":                 false,
			"answer_with_suggest":      false,
			"tts_switch":               false,
			"need_deep_think":          0,
			"click_clear_context":      false,
			"from_suggest":             false,
			"is_regen":                 false,
			"is_replace":               false,
			"is_from_click_option":     false,
			"is_from_click_softlink":   false,
			"disable_sse_cache":        false,
			"select_text_action":       "",
			"is_select_text":           false,
			"resend_for_regen":         false,
			"scene_type":               0,
			"unique_key":               uuid.NewString(),
			"start_seq":                0,
			"need_create_conversation": true,
			"conversation_init_option": map[string]any{"need_ack_conversation": true},
			"regen_query_id":           []any{},
			"edit_query_id":            []any{},
			"regen_instruction":        "",
			"no_replace_for_regen":     false,
			"message_from":             0,
			"shared_app_name":          "",
			"shared_app_id":            "",
			"sse_recv_event_options":   map[string]any{"support_chunk_delta": true},
			"is_ai_playground":         false,
			"is_old_user":              false,
			"recovery_option": map[string]any{
				"is_recovery": false, "req_create_time_sec": now.Unix(), "append_sse_event_scene": 0,
			},
			"message_storage_type":        0,
			"related_deleted_message_ids": map[string]any{},
			"connector_info_list":         []any{},
			"model_config":                map[string]any{"model_item_key": "", "model_extra_params": map[string]any{}},
			"aggregate_params": map[string]any{
				"conversation_mode": "", "mode_id": "", "model_item_key": "", "agent_mode": "", "reasoning_effort": "", "provider_id": "",
			},
		},
		"chat_ability": map[string]any{
			"ability_type":  17,
			"ability_param": string(abilityParam),
		},
		"user_context": []any{},
		"ext": map[string]any{
			"answer_with_suggest":           "0",
			"is_finish":                     "1",
			"sub_conv_firstmet_type":        "1",
			"collection_id":                 "",
			"conversation_init_option":      `{"need_ack_conversation":true}`,
			"commerce_credit_config_enable": "0",
		},
	}
}

func validRatio(ratio string) bool {
	ratio = strings.TrimSpace(ratio)
	if ratio == "" {
		return false
	}
	left, right, ok := strings.Cut(ratio, ":")
	if !ok || left == "" || right == "" {
		return false
	}
	if _, err := strconv.Atoi(left); err != nil {
		return false
	}
	if _, err := strconv.Atoi(right); err != nil {
		return false
	}
	return true
}
