package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"backend/internal/config"
	"backend/internal/model"
	"backend/internal/provider/byteplus"
	"gorm.io/datatypes"
)

func TestNormalizeImageResponseFormat(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "default is url", want: "url"},
		{name: "base64", input: "b64_json", want: "b64_json"},
		{name: "url", input: " URL ", want: "url"},
		{name: "invalid", input: "bytes", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeImageResponseFormat(tt.input)
			if tt.wantErr {
				if !errors.Is(err, ErrUnsupportedParams) {
					t.Fatalf("normalizeImageResponseFormat() error = %v, want ErrUnsupportedParams", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeImageResponseFormat() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("normalizeImageResponseFormat() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStoredImageResponseFormatPreservesRecoveryContract(t *testing.T) {
	if got := storedImageResponseFormat("b64_json"); got != "b64_json" {
		t.Fatalf("stored b64_json = %q", got)
	}
	for _, value := range []string{"", "url", "invalid-legacy-value"} {
		if got := storedImageResponseFormat(value); got != "url" {
			t.Fatalf("storedImageResponseFormat(%q) = %q, want url", value, got)
		}
	}
}

func TestRecoveredImageArtifactUsesActualAVIFTypeAndExtension(t *testing.T) {
	avif := append([]byte{0, 0, 0, 24}, []byte("ftypavif")...)
	avif = append(avif, make([]byte, 32)...)
	key, contentType, err := recoveredImageArtifact("api-owner/result.png", "evt-1", avif)
	if err != nil {
		t.Fatal(err)
	}
	if key != "api-owner/result.avif" || contentType != "image/avif" {
		t.Fatalf("recovered artifact = key %q, type %q", key, contentType)
	}
}

func TestCanonicalImageInputDefaultsNToOne(t *testing.T) {
	canonical, _, err := canonicalImageIdempotencyInput(V1ImageRequest{Model: "gpt-image-2", Prompt: "fox"})
	if err != nil {
		t.Fatal(err)
	}
	if canonical.N != 1 {
		t.Fatalf("canonical n = %d, want 1", canonical.N)
	}
}

func TestPrepareMediaRejectsMissingAndIgnoredParametersAsBadRequest(t *testing.T) {
	svc := &V1Service{}
	imageCases := []V1ImageRequest{
		{Prompt: "fox"},
		{Model: "gpt-image-2"},
		{Model: "gpt-image-2", Prompt: "fox", N: 2},
		{Model: "gpt-image-2", Prompt: "fox", Background: "transparent"},
		{Model: "gpt-image-2", Prompt: "fox", OutputFormat: "webp"},
	}
	for _, request := range imageCases {
		if _, _, _, _, err := svc.prepareImage(context.Background(), nil, request, false); !errors.Is(err, ErrUnsupportedParams) {
			t.Fatalf("prepareImage(%#v) error = %v, want ErrUnsupportedParams", request, err)
		}
	}
	videoCases := []V1VideoRequest{{Prompt: "fox", Duration: "6s"}, {Model: "sora-2", Duration: "6s"}, {Model: "sora-2", Prompt: "fox"}}
	for _, request := range videoCases {
		if _, _, _, _, _, err := svc.prepareVideo(context.Background(), nil, request, false); !errors.Is(err, ErrUnsupportedParams) {
			t.Fatalf("prepareVideo(%#v) error = %v, want ErrUnsupportedParams", request, err)
		}
	}
}

func TestPublicImageContentURL(t *testing.T) {
	if got := publicImageContentURL("https://HOST:9445/", "evt-OPAQUE"); got != "https://HOST:9445/v1/images/evt-OPAQUE/content" {
		t.Fatalf("publicImageContentURL() = %q", got)
	}
	if got := publicImageContentURL("", "evt-OPAQUE"); got != "/v1/images/evt-OPAQUE/content" {
		t.Fatalf("relative publicImageContentURL() = %q", got)
	}
}

func TestOutputBaseURLPrefersConfiguredPublicOrigin(t *testing.T) {
	svc := &V1Service{cfg: &config.Config{PublicBaseURL: "https://api.example.test/"}}
	if got := svc.outputBaseURL("http://internal:2000"); got != "https://api.example.test" {
		t.Fatalf("outputBaseURL() = %q", got)
	}
}

func TestImageTaskURLUsesPublicOriginAndEscapesRequestID(t *testing.T) {
	svc := &V1Service{cfg: &config.Config{PublicBaseURL: "https://api.example.test/"}}
	if got := svc.imageTaskURL("request id/1", "http://internal:2000"); got != "https://api.example.test/v1/images/tasks?request_id=request+id%2F1" {
		t.Fatalf("imageTaskURL() = %q", got)
	}
}

func TestAsyncImageJobResponseLifecycle(t *testing.T) {
	job := newAsyncImageJob("fingerprint")
	response, pending := asyncImageJobResponse(job, "req-1", "/poll")
	if !pending || response["status"] != "queued" {
		t.Fatalf("pending response = %#v, pending=%v", response, pending)
	}

	job.complete(map[string]any{"data": []map[string]any{{"url": "https://example.test/image.png"}}}, nil)
	response, pending = asyncImageJobResponse(job, "req-1", "/poll")
	if pending || response["status"] != "completed" || response["request_id"] != "req-1" {
		t.Fatalf("completed response = %#v, pending=%v", response, pending)
	}
}

func TestAsyncImageJobResponseFailure(t *testing.T) {
	job := newAsyncImageJob("fingerprint")
	job.complete(nil, errors.New("render failed Authorization: Bearer sk-secret Cookie=sessionid-secret"))
	response, pending := asyncImageJobResponse(job, "req-2", "/poll")
	if pending || response["status"] != "failed" || response["error"] != ErrProviderExecution.Error() {
		t.Fatalf("failed response = %#v, pending=%v", response, pending)
	}
}

func TestAsyncImageJobResponseKeepsAcceptedTaskPending(t *testing.T) {
	job := newAsyncImageJob("fingerprint")
	job.complete(nil, errors.Join(ErrProviderTemporary, byteplus.ErrTaskAccepted))
	response, pending := asyncImageJobResponse(job, "req-accepted", "/poll")
	if !pending || response["status"] != "queued" {
		t.Fatalf("accepted response = %#v, pending=%v", response, pending)
	}
}

func TestWaitForImageResultReturnsDurablePendingImmediately(t *testing.T) {
	svc := &V1Service{}
	pending := map[string]any{
		"id": "req-accepted", "request_id": "req-accepted", "status": "in_progress", "poll_url": "/v1/images/tasks?request_id=req-accepted",
	}
	started := time.Now()
	response, err := svc.waitForImageResult(context.Background(), nil, "req-accepted", "fingerprint", pending)
	if !errors.Is(err, ErrGenerationAccepted) {
		t.Fatalf("waitForImageResult() error = %v, want ErrGenerationAccepted", err)
	}
	if response["request_id"] != "req-accepted" || response["status"] != "in_progress" || response["poll_url"] == "" {
		t.Fatalf("pending response = %#v", response)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("durable pending response waited %s", elapsed)
	}
}

func TestModelPriceTextPerRequest(t *testing.T) {
	item := &model.ModelConfig{
		Prices:      datatypes.JSONMap{"request": 2.5},
		PricesAgent: datatypes.JSONMap{"request": 1.5},
	}
	if got, ok := modelPrice(item, "text", "", "", false); !ok || got != 2.5 {
		t.Fatalf("normal text price = %v, %v", got, ok)
	}
	if got, ok := modelPrice(item, "text", "", "", true); !ok || got != 1.5 {
		t.Fatalf("agent text price = %v, %v", got, ok)
	}
}

func TestChatAccountingBodyRequiresDoneForStream(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantOK  bool
		wantBad bool
	}{
		{name: "done", body: "data: {}\n\ndata: [DONE]\n\n", wantOK: true},
		{name: "compact done", body: "data: {}\n\ndata:[DONE]\n\n", wantOK: true},
		{name: "truncated", body: "data: {}\n\n", wantBad: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ok, bad bool
			body := &chatAccountingBody{
				inner:  io.NopCloser(strings.NewReader(tt.body)),
				stream: true,
				finish: func(success bool, _ string, upstreamFailure bool) { ok, bad = success, upstreamFailure },
			}
			_, _ = io.ReadAll(body)
			_ = body.Close()
			if ok != tt.wantOK || bad != tt.wantBad {
				t.Fatalf("finish = ok:%v bad:%v, want ok:%v bad:%v", ok, bad, tt.wantOK, tt.wantBad)
			}
		})
	}
}

func TestOpenAITextResponseAndTranscript(t *testing.T) {
	header, body := openAITextResponse("chatgpt-auto", "hello", true)
	defer body.Close()
	raw, _ := io.ReadAll(body)
	if header.Get("Content-Type") != "text/event-stream" || !strings.Contains(string(raw), `"object":"chat.completion.chunk"`) || !strings.Contains(string(raw), "data: [DONE]") {
		t.Fatalf("unexpected SSE response: header=%v body=%s", header, raw)
	}
	prompt := chatPrompt([]any{
		map[string]any{"role": "system", "content": "be brief"},
		map[string]any{"role": "user", "content": "hi"},
	})
	if prompt != "SYSTEM: be brief\nUSER: hi" {
		t.Fatalf("chatPrompt() = %q", prompt)
	}
}
