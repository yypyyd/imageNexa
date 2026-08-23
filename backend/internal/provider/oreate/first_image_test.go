package oreate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseImageSSE(t *testing.T) {
	body := "data: {\"event\":\"start\",\"logId\":\"1\"}\n\n" +
		"data: {\"event\":\"data\",\"content\":\"![image](https:\\/\\/cdn.oreateai.com\\/aiimage/kling/abc/def)\"}\n\n" +
		"data: {\"event\":\"end\",\"logId\":\"1\"}\n"
	got, err := parseImageSSE(strings.NewReader(body))
	if err != nil || got != "https://cdn.oreateai.com/aiimage/kling/abc/def" {
		t.Fatalf("parseImageSSE() = %q, %v", got, err)
	}
}

func TestParseImageSSEErrors(t *testing.T) {
	spam := "data: {\"event\":\"error\",\"data\":{\"code\":212361,\"msg\":\"spam user\"}}\n"
	if _, err := parseImageSSE(strings.NewReader(spam)); !errors.Is(err, ErrSpamUser) {
		t.Fatalf("spam error = %v", err)
	}
	dropped := "data: {\"event\":\"start\",\"logId\":\"1\"}\n"
	if _, err := parseImageSSE(strings.NewReader(dropped)); !errors.Is(err, errStreamIncomplete) {
		t.Fatalf("dropped stream error = %v", err)
	}
}

func TestClaimFirstImageBonus(t *testing.T) {
	var request imageRequest
	var streamCookie string
	bonusCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oreate/create/chat":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"aiImage"`) {
				t.Errorf("create chat body = %q", body)
			}
			_, _ = io.WriteString(w, `{"status":{"code":0},"data":{"chatId":"chat-img"}}`)
		case "/oreate/sse/stream":
			streamCookie = r.Header.Get("Cookie")
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode image request: %v", err)
			}
			_, _ = io.WriteString(w, "data: {\"event\":\"data\",\"content\":\"![image](https://cdn.example/first)\"}\n")
		case "/oreate/account/getfirstusepoint":
			bonusCalls++
			_, _ = io.WriteString(w, `{"status":{"code":0},"data":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewClient("")
	client.baseURL = server.URL
	client.SetSigner(stubSigner{sig: Signature{JT: "signed", BID: "browser-bid", Cookie: "_ga=GA1; __bid_n=fresh; OUID=browser-ouid"}})
	url, err := client.ClaimFirstImageBonus(context.Background(), Account{Cookie: "OUID=device-1; ouss=session", Email: "user@example.com"})
	if err != nil || url != "https://cdn.example/first" {
		t.Fatalf("ClaimFirstImageBonus() = %q, %v", url, err)
	}
	if bonusCalls != 1 {
		t.Fatalf("first-use bonus calls = %d", bonusCalls)
	}
	if request.ChatType != "aiImage" || request.ChatID != "chat-img" || request.ImageConfig.ModelName != firstImageModel || request.ImageConfig.Resolution != firstImageResolution {
		t.Fatalf("request = %#v", request)
	}
	if request.JT != "signed" || request.Extra.BID != "browser-bid" || request.Extra.DeviceID != "device-1" {
		t.Fatalf("request identity = %#v", request)
	}
	// The request carries the tracking cookies the signer browser minted on top of
	// the stored auth cookies; stored values win on name conflicts.
	if streamCookie != "OUID=device-1; ouss=session; _ga=GA1; __bid_n=fresh" {
		t.Fatalf("stream cookie = %q", streamCookie)
	}
}

// A stream that never yields an image still has to ask for the grant: the site
// has usually rendered and charged for the image the request paid for.
func TestClaimFirstImageBonusRequestsGrantAfterBrokenStream(t *testing.T) {
	bonusCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oreate/create/chat":
			_, _ = io.WriteString(w, `{"status":{"code":0},"data":{"chatId":"chat-img"}}`)
		case "/oreate/sse/stream":
			_, _ = io.WriteString(w, "data: {\"event\":\"start\",\"logId\":\"9\"}\n")
		case "/oreate/account/getfirstusepoint":
			bonusCalls++
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewClient("")
	client.baseURL = server.URL
	client.SetSigner(stubSigner{sig: Signature{JT: "signed"}})
	if _, err := client.ClaimFirstImageBonus(context.Background(), Account{Cookie: "ouss=session"}); !errors.Is(err, errStreamIncomplete) {
		t.Fatalf("ClaimFirstImageBonus() error = %v", err)
	}
	if bonusCalls != 1 {
		t.Fatalf("first-use bonus calls = %d", bonusCalls)
	}
}
