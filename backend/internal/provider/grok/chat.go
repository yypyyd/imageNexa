package grok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// GenerateText runs an ordinary grok web chat turn and returns the assistant's
// full reply. Same transport as GenerateImage: a chat submit to
// /rest/app-chat/conversations/new whose streamed frames carry the tokens; the
// final modelResponse.message is the authoritative full text.
//
// mode selects the upstream chat mode ("fast", "auto", "expert", "heavy");
// "fast" is the only mode a free (Basic) account can run — the others need
// SuperGrok and answer with a subscription error otherwise.
func (c *Client) GenerateText(ctx context.Context, token, prompt, mode string) (string, error) {
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	if token == "" {
		return "", ErrAuth
	}
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("grok: prompt required")
	}
	if mode = strings.TrimSpace(mode); mode == "" {
		mode = "fast"
	}

	submitClient, err := c.newSubmitTLSClient(token)
	if err != nil {
		return "", err
	}
	c.ensureChallenge(ctx, submitClient, token)

	payload := map[string]any{
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
		"disableTextFollowUps":        true,
		"enableImageGeneration":       false,
		"enableImageStreaming":        false,
		"enableSideBySide":            true,
		"fileAttachments":             []any{},
		"forceConcise":                false,
		"forceSideBySide":             false,
		"imageAttachments":            []any{},
		"imageGenerationCount":        0,
		"isAsyncChat":                 false,
		"message":                     prompt,
		"modeId":                      mode,
		"responseMetadata":            map[string]any{},
		"returnImageBytes":            false,
		"returnRawGrokInXaiRequest":   false,
		"sendFinalMetadata":           true,
		"temporary":                   true,
	}

	// body may be partial when psErr != nil — usable text often already arrived.
	body, psErr := c.postStream(ctx, submitClient, token, "/rest/app-chat/conversations/new", payload)
	if strings.Contains(body, "usagePoolExhausted") || strings.Contains(body, "usage limit") {
		return "", fmt.Errorf("%w: chat usage limit reached", ErrQuotaExhausted)
	}
	if psErr != nil && (errors.Is(psErr, ErrAuth) || errors.Is(psErr, ErrQuotaExhausted)) {
		return "", psErr
	}
	text, streamErr := chatResponseText(body)
	if text == "" {
		if streamErr != nil {
			return "", fmt.Errorf("%w: %v", ErrTemporaryUpstream, streamErr)
		}
		if psErr != nil {
			return "", psErr
		}
		return "", fmt.Errorf("%w: empty chat response: %s", ErrTemporaryUpstream, clip([]byte(body), 200))
	}
	return text, nil
}

// chatResponseText extracts the assistant reply out of a chat stream: the run of
// concatenated JSON frames carries incremental result.response.token values
// (isThinking tokens are reasoning, tool_usage_card tokens are internal XML —
// both skipped) and usually a final result.response.modelResponse.message with
// the complete text, which wins when present. A stream error frame is returned
// alongside so callers can surface it when no text arrived.
func chatResponseText(body string) (string, error) {
	dec := json.NewDecoder(strings.NewReader(body))
	var tokens strings.Builder
	full := ""
	var streamErr error
	noteErr := func(value map[string]any) {
		if streamErr != nil || value == nil {
			return
		}
		if message := strings.TrimSpace(stringValue(value["message"])); message != "" {
			streamErr = errors.New(message)
		}
	}
	for {
		var frame map[string]any
		if err := dec.Decode(&frame); err != nil {
			break
		}
		if errValue, _ := frame["error"].(map[string]any); errValue != nil {
			noteErr(errValue)
			continue
		}
		result, _ := frame["result"].(map[string]any)
		response, _ := result["response"].(map[string]any)
		if response == nil {
			continue
		}
		if errValue, _ := response["error"].(map[string]any); errValue != nil {
			noteErr(errValue)
			continue
		}
		if modelResponse, _ := response["modelResponse"].(map[string]any); modelResponse != nil {
			if message := strings.TrimSpace(stringValue(modelResponse["message"])); message != "" && full == "" {
				full = message
			}
			continue
		}
		token := stringValue(response["token"])
		if token == "" {
			continue
		}
		if thinking, _ := response["isThinking"].(bool); thinking {
			continue
		}
		if tag := stringValue(response["messageTag"]); tag != "" && tag != "final" {
			continue
		}
		tokens.WriteString(token)
	}
	if full != "" {
		return full, streamErr
	}
	return strings.TrimSpace(tokens.String()), streamErr
}

// ChatModeForModel maps an upstream model name ("grok-chat-expert", "grok-4",
// …) onto the web chat modeId. Unknown names run "fast" — the only mode a free
// account has.
func ChatModeForModel(upstreamModel string) string {
	name := strings.ToLower(strings.TrimSpace(upstreamModel))
	for _, mode := range []string{"expert", "heavy", "auto"} {
		if strings.Contains(name, mode) {
			return mode
		}
	}
	return "fast"
}
