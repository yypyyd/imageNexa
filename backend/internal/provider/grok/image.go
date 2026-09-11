package grok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// GenerateImage runs grok's "Lite" (fast mode) text-to-image pipeline. Unlike the
// video path there is no media post / dedicated endpoint: it is an ordinary chat
// submit to /rest/app-chat/conversations/new with enableImageGeneration and a
// "Drawing: <prompt>" message; the streamed response carries the rendered image
// paths (relative to assets.grok.com).
//
// Lite is the only media generation a free (Basic) grok account can run — the
// Imagine HD image model and imagine video both require SuperGrok. Upstream
// always renders imageGenerationCount images per submit while charging one Fast
// unit; we return the first one. When downloadResult is false, returns nil bytes
// and the artifact URL in meta["image_url"]; otherwise downloads the image.
func (c *Client) GenerateImage(ctx context.Context, token, prompt, aspectRatio string, downloadResult bool) ([]byte, map[string]any, error) {
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	if token == "" {
		return nil, nil, ErrAuth
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, nil, fmt.Errorf("grok: prompt required")
	}
	aspectRatio = normalizeImageAspectRatio(aspectRatio)

	// Account bootstrap/challenge and the conversations/new submit use the proxy;
	// media and artifact transfers remain direct.
	submitClient, err := c.newSubmitTLSClient(token)
	if err != nil {
		return nil, nil, err
	}
	directClient, err := c.newDirectTLSClient()
	if err != nil {
		return nil, nil, err
	}
	c.ensureChallenge(ctx, submitClient, token)

	// The web client's own conversation payload; modeId "fast" is Lite.
	payload := map[string]any{
		// The current Grok Imagine API reads image generation options from
		// mediaGenInput.  Keeping the explicit modelName is important: the older
		// enableImageGeneration flag alone silently falls back to a square image.
		"modelName":            "imagine-image-gen",
		"collectionIds":        []any{},
		"disabledConnectorIds": []any{},
		"deviceEnvInfo": map[string]any{
			"darkModeEnabled": false, "devicePixelRatio": 2,
			"screenHeight": 1328, "screenWidth": 2056,
			"viewportHeight": 1083, "viewportWidth": 2056,
		},
		"disableMemory":               true,
		"disableSearch":               false,
		"disableSelfHarmShortCircuit": false,
		"disableTextFollowUps":        false,
		"enableImageGeneration":       true,
		"enableImageStreaming":        true,
		"enableSideBySide":            true,
		"fileAttachments":             []any{},
		"forceConcise":                false,
		"forceSideBySide":             false,
		"imageAttachments":            []any{},
		"imageGenerationCount":        2,
		"isAsyncChat":                 false,
		"message":                     "Drawing: " + prompt,
		"mediaGenInput": map[string]any{
			"textToImage": map[string]any{
				"prompt":      prompt,
				"numOfImages": 2,
				// conversations/new declares aspectRatio as a protobuf string.
				// Do not pass the UI/catalog ratio pair (e.g. [16, 9]) here: the
				// upstream parser rejects it with "invalid value for string field
				// aspectRatio: [".
				"aspectRatio":    aspectRatio,
				"resolutionName": "1k",
				"mode":           "fast",
			},
		},
		"modeId": "fast",
		"responseMetadata": map[string]any{
			"modelConfigOverride": map[string]any{
				"modelMap": map[string]any{"imageEditModel": "imagine"},
			},
		},
		"returnImageBytes":          false,
		"returnRawGrokInXaiRequest": false,
		"sendFinalMetadata":         true,
		"temporary":                 true,
	}

	// body may be partial when psErr != nil — the artifact often already appeared.
	body, psErr := c.postStream(ctx, submitClient, token, "/rest/app-chat/conversations/new", payload)
	if strings.Contains(body, "usagePoolExhausted") || strings.Contains(body, "media generation credits") {
		return nil, nil, fmt.Errorf("%w: media generation credits exhausted", ErrQuotaExhausted)
	}
	if psErr != nil && (errors.Is(psErr, ErrAuth) || errors.Is(psErr, ErrQuotaExhausted)) {
		return nil, nil, psErr
	}
	artifact := firstGeneratedImage(body)
	if artifact == "" {
		if strings.Contains(body, "STREAM_ERROR_SEVERITY_FATAL") {
			return nil, nil, fmt.Errorf("grok: image generation rejected by upstream (fatal stream error): %s", clip([]byte(body), 200))
		}
		if psErr != nil {
			return nil, nil, psErr
		}
		return nil, nil, fmt.Errorf("%w: no image artifact in response: %s", ErrTemporaryUpstream, clip([]byte(body), 200))
	}
	fullURL := artifact
	if !strings.HasPrefix(fullURL, "http") {
		fullURL = assetBase + strings.TrimPrefix(artifact, "/")
	}

	meta := map[string]any{
		"provider":  "grok",
		"image_url": fullURL,
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

// normalizeImageAspectRatio mirrors Grok Imagine's fast-mode v1 set.  The
// service clamps user-facing sizes against the model catalog before calling
// this method; this final guard prevents an invalid value from reaching Grok
// when the provider is used directly.
func normalizeImageAspectRatio(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "x", ":"))
	for _, allowed := range []string{"2:3", "3:2", "1:1", "9:16", "16:9"} {
		if value == allowed {
			return value
		}
	}
	return "1:1"
}

// firstGeneratedImage picks the finished image path out of a Lite chat stream.
// The stream is a run of concatenated JSON frames and the rendered image arrives
// in one of three shapes depending on the grok build:
//
//	response.cardAttachment.jsonData          -> {"image_chunk":{"imageUrl":…,"progress":100}}
//	response.streamingImageGenerationResponse -> {"imageUrl":…,"progress":100}
//	response.modelResponse                    -> generatedImageUrls / cardAttachmentsJson
//
// Only a completed chunk counts (progress 100 / isFinal): earlier frames point at
// a "-part-N" preview. Moderated paths are dropped — grok reports them as
// completed but never renders them.
func firstGeneratedImage(body string) string {
	dec := json.NewDecoder(strings.NewReader(body))
	moderated := map[string]struct{}{}
	streamed := ""
	// keep records the first completed, non-moderated path of a chunk frame.
	keep := func(chunk map[string]any) {
		url := strings.TrimSpace(stringValue(chunk["imageUrl"]))
		if url == "" {
			url = strings.TrimSpace(stringValue(chunk["url"]))
		}
		if url == "" {
			return
		}
		if flag, _ := chunk["moderated"].(bool); flag {
			moderated[url] = struct{}{}
			return
		}
		final, _ := chunk["isFinal"].(bool)
		progress, ok := chunk["progress"].(float64)
		if !final && !(ok && progress >= 100) {
			return
		}
		if streamed == "" {
			streamed = url
		}
	}
	for {
		var frame map[string]any
		if err := dec.Decode(&frame); err != nil {
			break
		}
		result, _ := frame["result"].(map[string]any)
		response, _ := result["response"].(map[string]any)
		if response == nil {
			continue
		}
		if card, _ := response["cardAttachment"].(map[string]any); card != nil {
			keep(cardImageChunk(stringValue(card["jsonData"])))
		}
		if img, _ := response["streamingImageGenerationResponse"].(map[string]any); img != nil {
			keep(img)
		}
		modelResponse, _ := response["modelResponse"].(map[string]any)
		if modelResponse == nil {
			continue
		}
		cards, _ := modelResponse["cardAttachmentsJson"].([]any)
		for _, raw := range cards {
			keep(cardImageChunk(stringValue(raw)))
		}
		urls, _ := modelResponse["generatedImageUrls"].([]any)
		for _, raw := range urls {
			url := strings.TrimSpace(stringValue(raw))
			if url == "" {
				continue
			}
			if _, bad := moderated[url]; bad {
				continue
			}
			return url
		}
	}
	if _, bad := moderated[streamed]; bad {
		return ""
	}
	return streamed
}

// cardImageChunk unwraps the image_chunk of a generated_image_card's embedded
// json payload (nil for any other card type).
func cardImageChunk(jsonData string) map[string]any {
	if strings.TrimSpace(jsonData) == "" {
		return nil
	}
	var card map[string]any
	if err := json.Unmarshal([]byte(jsonData), &card); err != nil {
		return nil
	}
	chunk, _ := card["image_chunk"].(map[string]any)
	return chunk
}
