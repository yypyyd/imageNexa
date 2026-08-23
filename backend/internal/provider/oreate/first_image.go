package oreate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// Oreate awards a new account 50 bonus credits for its first successful image
// generation, and only releases them when the site's first-use endpoint asks
// for the grant. An imported account claims the award once on the cheapest
// image tier (6 credits) so the pool never holds half-funded accounts.
const (
	firstImageModel      = "Kling3.0 Omini"
	firstImageResolution = "1k"
	firstImageRatio      = "1:1"
	firstImagePrompt     = "a cute orange cat sitting on a wooden desk, soft light"
)

// Rendered images arrive as markdown in the stream's message content, and their
// CDN paths carry no file extension.
var imageMarkdownPattern = regexp.MustCompile(`!\[[^\]]*\]\((https?://[^\s)]+)\)`)

type imageRequest struct {
	ClientType  string         `json:"clientType"`
	Type        string         `json:"type"`
	FocusID     string         `json:"focusId"`
	ChatID      string         `json:"chatId"`
	ChatType    string         `json:"chatType"`
	From        string         `json:"from"`
	ChatTitle   string         `json:"chatTitle"`
	IsFirst     bool           `json:"isFirst"`
	Messages    []videoMessage `json:"messages"`
	ImageConfig imageConfig    `json:"imageConfig"`
	JT          string         `json:"jt"`
	UA          string         `json:"ua"`
	JSEnv       string         `json:"js_env"`
	Extra       requestExtra   `json:"extra"`
}

type imageConfig struct {
	ModelName  string `json:"modelName"`
	Ratio      string `json:"ratio"`
	Resolution string `json:"resolution"`
}

// ClaimFirstImageBonus generates one low-tier image and then asks Oreate to
// release the first-use bonus it earns. The grant is requested even when the
// stream broke: the site has usually rendered the image and charged for it
// anyway, so the account would otherwise keep paying without the award.
func (c *Client) ClaimFirstImageBonus(ctx context.Context, account Account) (string, error) {
	account = account.normalized()
	if account.Cookie == "" {
		return "", ErrAuth
	}
	if c.signer == nil {
		return "", errors.New("oreate: signer not configured")
	}
	// The token only works from the exit IP that minted it, so the chat and the
	// stream both leave through the signing page's proxy session.
	sig, err := c.signHot(ctx, account)
	if err != nil {
		return "", err
	}
	chatID, err := c.createChat(ctx, account, "aiImage", sig.Proxy)
	if err != nil {
		return "", err
	}
	if sig.BID != "" {
		account.BID = sig.BID
	}
	if sig.Cookie != "" {
		account.Cookie = mergeCookies(account.Cookie, sig.Cookie)
	}
	payload := imageRequest{
		ClientType: "pc", Type: "chat", FocusID: chatID, ChatID: chatID,
		ChatType: "aiImage", From: "home", ChatTitle: "Unnamed Session", IsFirst: true,
		Messages:    []videoMessage{{Role: "user", Content: firstImagePrompt, Attachments: []videoAttachment{}}},
		ImageConfig: imageConfig{ModelName: firstImageModel, Ratio: firstImageRatio, Resolution: firstImageResolution},
		JT:          sig.JT, UA: account.UserAgent, JSEnv: "h5",
		Extra: requestExtra{
			DocName: "", ModuleName: "gpt4o", Email: account.Email, VIP: account.VIP,
			RegTS: account.RegTS, DeviceID: account.OUID, BID: account.BID,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/oreate/sse/stream"), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	setHeaders(req, account, "text/event-stream")
	resp, err := c.egressClient(sig.Proxy).Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: image stream request: %v", ErrTemporaryUpstream, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", ErrAuth
	}
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", classifyUpstreamError(resp.StatusCode, string(message))
	}
	imageURL, streamErr := parseImageSSE(resp.Body)
	c.requestFirstUseBonus(ctx, account, sig.Proxy)
	return imageURL, streamErr
}

// requestFirstUseBonus triggers the grant the way the site's own frontend does
// after a generation. A failure only leaves the award unpaid, so it is ignored.
func (c *Client) requestFirstUseBonus(ctx context.Context, account Account, proxyURL string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/oreate/account/getfirstusepoint"), nil)
	if err != nil {
		return
	}
	setHeaders(req, account, "application/json")
	resp, err := c.egressClient(proxyURL).Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
}

func parseImageSSE(r io.Reader) (string, error) {
	scanner := newStreamScanner(r)
	imageURL := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.ReplaceAll(strings.TrimSpace(strings.TrimPrefix(line, "data:")), `\/`, "/")
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if match := imageMarkdownPattern.FindStringSubmatch(payload); len(match) == 2 {
			imageURL = match[1]
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		if strings.EqualFold(scalarString(event["event"], ""), "error") {
			data, _ := event["data"].(map[string]any)
			msg := scalarString(data["msg"], scalarString(data["message"], "upstream error"))
			return imageURL, classifyUpstreamError(intFromAny(data["code"]), msg)
		}
	}
	if err := scanner.Err(); err != nil {
		return imageURL, fmt.Errorf("%w: %w: read stream: %v", ErrTemporaryUpstream, errStreamIncomplete, err)
	}
	if imageURL == "" {
		return "", fmt.Errorf("%w: %w", ErrTemporaryUpstream, errStreamIncomplete)
	}
	return imageURL, nil
}
