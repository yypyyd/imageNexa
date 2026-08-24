package oreate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type stubSigner struct {
	sig Signature
	err error
}

func (s stubSigner) Sign(context.Context, Account) (Signature, error) { return s.sig, s.err }

// stubSubmitter stands in for the signed page: the real submit only ever happens
// in a browser, so the stub captures the request Go built and replays a canned
// event stream.
type stubSubmitter struct {
	stubSigner
	chatID  string
	stream  string
	status  int
	payload []byte
}

func (s *stubSubmitter) SubmitVideo(_ context.Context, _ Account, payload []byte) (videoSubmitResult, error) {
	s.payload = payload
	status := s.status
	if status == 0 {
		status = http.StatusOK
	}
	return videoSubmitResult{Status: status, ChatID: s.chatID, Stream: s.stream, Done: true}, nil
}

func (s *stubSubmitter) request(t *testing.T) videoRequest {
	t.Helper()
	var request videoRequest
	if err := json.Unmarshal(s.payload, &request); err != nil {
		t.Fatalf("decode submitted request: %v", err)
	}
	return request
}

func TestParseCreateChat(t *testing.T) {
	id, err := parseCreateChat([]byte(`{"status":{"code":0,"msg":"success"},"data":{"chatId":" chat-1 "}}`))
	if err != nil || id != "chat-1" {
		t.Fatalf("parseCreateChat() = %q, %v", id, err)
	}
	for _, body := range []string{`not-json`, `{"status":{"code":0},"data":{}}`} {
		if _, err := parseCreateChat([]byte(body)); !errors.Is(err, ErrTemporaryUpstream) {
			t.Fatalf("parseCreateChat(%q) error = %v", body, err)
		}
	}
	if _, err := parseCreateChat([]byte(`{"status":{"code":200017,"msg":"point exceed"}}`)); !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("quota error = %v", err)
	}
}

func TestParseVideoSSE(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"direct", "data: {\"event\":\"start\"}\n\ndata: {\"event\":\"end\",\"data\":{\"videoUrl\":\"https://cdn.example/out.mp4\"}}\n", "https://cdn.example/out.mp4"},
		{"nested-json", "data: {\"event\":\"data\",\"data\":\"{\\\"result\\\":{\\\"url\\\":\\\"https:\\\\/\\\\/cdn.example\\\\/nested.mp4?x=1\\\"}}\"}\n", "https://cdn.example/nested.mp4?x=1"},
		{"markdown", "data: {\"event\":\"end\",\"content\":\"[video](https://cdn.example/md.mp4)\"}\n", "https://cdn.example/md.mp4"},
		{"html", "data: <video src=\"https://cdn.example/html.mp4\"></video>\n", "https://cdn.example/html.mp4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseVideoSSE(strings.NewReader(tt.body))
			if err != nil || got.VideoURL != tt.want {
				t.Fatalf("parseVideoSSE() = %q, %v; want %q", got.VideoURL, err, tt.want)
			}
		})
	}
}

func TestParseVideoSSEError(t *testing.T) {
	for _, tt := range []struct {
		code int
		msg  string
		want error
	}{
		{212361, "spam user", ErrRiskControl},
		{200017, "point exceed", ErrQuotaExhausted},
		{401, "unauthenticated", ErrAuth},
		{500, "busy", ErrTemporaryUpstream},
		{0, "content policy", ErrContentRejected},
	} {
		body := fmt.Sprintf("data: {\"event\":\"error\",\"data\":{\"code\":%d,\"msg\":%q}}\n", tt.code, tt.msg)
		_, err := parseVideoSSE(strings.NewReader(body))
		if !errors.Is(err, tt.want) {
			t.Errorf("code %d error = %v, want %v", tt.code, err, tt.want)
		}
		if tt.code == 212361 && !errors.Is(err, ErrSpamUser) {
			t.Errorf("code %d error = %v, want ErrSpamUser", tt.code, err)
		}
	}
}

func TestParseVideoSSEIncompleteStream(t *testing.T) {
	body := "data: {\"event\":\"start\",\"logId\":\"2098276034\"}\n\ndata: {\"event\":\"generating\",\"logId\":\"2098276034\"}\n"
	got, err := parseVideoSSE(strings.NewReader(body))
	if !errors.Is(err, errStreamIncomplete) || !errors.Is(err, ErrTemporaryUpstream) {
		t.Fatalf("parseVideoSSE() error = %v", err)
	}
	if !got.Started || got.LogID != "2098276034" || got.VideoURL != "" {
		t.Fatalf("parseVideoSSE() = %#v", got)
	}
}

// A stream that drops after the job was accepted must still hand back the
// rendered file: the submit already spent the account's credits.
func TestGenerateVideoRecoversDroppedStreamByLogID(t *testing.T) {
	cdnHits := 0
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/777.mp4" {
			http.NotFound(w, r)
			return
		}
		cdnHits++
		_, _ = w.Write([]byte("recovered-mp4"))
	}))
	defer cdn.Close()
	// An unreadable chat is what leaves logId recovery as the only way to the
	// rendered file.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()
	client := NewClient("")
	client.baseURL = server.URL
	client.cdnBaseURL = cdn.URL
	client.signer = &stubSubmitter{chatID: "chat-3", stream: "data: {\"event\":\"start\",\"logId\":\"777\"}\n"}
	data, meta, err := client.GenerateVideo(context.Background(), Account{Cookie: "ouss=x"}, VideoOptions{
		ModelID: "seedance-2.0-mini", Prompt: "hello", Ratio: "16:9", Resolution: "480p",
		Duration: 5, DownloadResult: true,
	})
	if err != nil || string(data) != "recovered-mp4" || cdnHits == 0 {
		t.Fatalf("GenerateVideo() = %q, %v", data, err)
	}
	if meta["video_url"] != cdn.URL+"/777.mp4" || meta["log_id"] != "777" {
		t.Fatalf("meta = %#v", meta)
	}
}

// Without a logId there is nothing to recover, and an upstream verdict such as
// a spam-user rejection must never be retried as a recovery.
func TestGenerateVideoSkipsRecoveryWithoutRecoverableJob(t *testing.T) {
	streams := map[string]string{
		"no-log-id": "data: {\"event\":\"start\"}\n",
		"spam-user": "data: {\"event\":\"start\",\"logId\":\"777\"}\n\ndata: {\"event\":\"error\",\"logId\":\"777\",\"data\":{\"code\":212361,\"msg\":\"spam user\"}}\n",
	}
	for name, stream := range streams {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.NotFound(w, r)
			}))
			defer server.Close()
			cdnCalls := 0
			cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				cdnCalls++
				_, _ = w.Write([]byte("unexpected"))
			}))
			defer cdn.Close()
			client := NewClient("")
			client.baseURL = server.URL
			client.cdnBaseURL = cdn.URL
			client.signer = &stubSubmitter{chatID: "chat-4", stream: stream}
			_, _, err := client.GenerateVideo(context.Background(), Account{Cookie: "ouss=x"}, VideoOptions{
				ModelID: "seedance-2.0-mini", Prompt: "hello", Ratio: "16:9", Resolution: "480p", Duration: 5,
			})
			if err == nil || cdnCalls != 0 {
				t.Fatalf("GenerateVideo() error = %v, cdn calls = %d", err, cdnCalls)
			}
			if name == "spam-user" && !errors.Is(err, ErrSpamUser) {
				t.Fatalf("GenerateVideo() error = %v, want ErrSpamUser", err)
			}
		})
	}
}

// The page owns the identity fields, so the request Go hands it carries the
// model configuration and the content while jt, chatId and focusId stay empty.
func TestGenerateVideoRequestAndDownload(t *testing.T) {
	video := []byte("test-mp4")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/video.mp4" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(video)
	}))
	defer server.Close()

	client := NewClient("")
	client.baseURL = server.URL
	submitter := &stubSubmitter{
		chatID: "chat-1",
		stream: fmt.Sprintf("data: {\"event\":\"end\",\"data\":{\"url\":%q}}\n", server.URL+"/video.mp4"),
	}
	client.signer = submitter
	account := Account{Cookie: "OUID=device-1; ouss=session", UserAgent: "test-agent", Email: "user@example.com", VIP: "0", RegTS: 123}
	data, meta, err := client.GenerateVideo(context.Background(), account, VideoOptions{
		ModelID: "seedance-2.0-mini", Prompt: "hello", Ratio: "16:9", Resolution: "480p",
		Duration: 5, Audio: false, DownloadResult: true,
	})
	if err != nil {
		t.Fatalf("GenerateVideo() error = %v", err)
	}
	if string(data) != string(video) || meta["video_url"] == "" {
		t.Fatalf("GenerateVideo() data/meta = %q, %#v", data, meta)
	}
	gotRequest := submitter.request(t)
	if gotRequest.JT != "" || gotRequest.ChatID != "" || gotRequest.FocusID != "" {
		t.Fatalf("request identity = %#v", gotRequest)
	}
	if gotRequest.ChatType != "aiVideo" || gotRequest.VideoConfig.AIType != 14198 || gotRequest.VideoConfig.Scene != "text_or_image" {
		t.Fatalf("request config = %#v", gotRequest)
	}
	if gotRequest.UA != "test-agent" || gotRequest.Extra.DeviceID != "device-1" || len(gotRequest.Messages) != 1 || gotRequest.Messages[0].Content != "hello" {
		t.Fatalf("request content = %#v", gotRequest)
	}
}

func TestGenerateVideoURLOnly(t *testing.T) {
	client := NewClient("")
	client.signer = &stubSubmitter{chatID: "chat-2", stream: "data: {\"event\":\"end\",\"url\":\"https://cdn.example/url-only.mp4\"}\n"}
	data, meta, err := client.GenerateVideo(context.Background(), Account{Cookie: "ouss=x"}, VideoOptions{
		ModelID: "seedance-2.0-fast", Prompt: "hello", Ratio: "1:1", Resolution: "720",
		Duration: 10, Audio: true, DownloadResult: false,
	})
	if err != nil || data != nil || meta["video_url"] != "https://cdn.example/url-only.mp4" {
		t.Fatalf("GenerateVideo() = %q, %#v, %v", data, meta, err)
	}
}

func TestGenerateVideoReferenceScenes(t *testing.T) {
	tests := []struct {
		name        string
		options     VideoOptions
		wantScene   string
		wantAIType  int
		wantImages  int
		wantVideos  int
		wantRefBand string
	}{
		{
			name: "seedance-2.5-image-reference",
			options: VideoOptions{
				ModelID: "seedance-2.5", Prompt: "animate", Ratio: "16:9", Resolution: "480p", Duration: 20, Audio: true,
				ReferenceImages: []MediaReference{{Data: []byte("png-data"), ContentType: "image/png"}},
			},
			wantScene: "reference", wantAIType: 14227, wantImages: 1,
		},
		{
			name: "seedance-1.5-two-frames",
			options: VideoOptions{
				ModelID: "seedance-1.5-pro", Prompt: "transition", Ratio: "1:1", Resolution: "720p", Duration: 5,
				ReferenceImages: []MediaReference{
					{Data: []byte("first"), ContentType: "image/jpeg"},
					{Data: []byte("last"), ContentType: "image/jpeg"},
				},
			},
			wantScene: "frame_based", wantAIType: 14003, wantImages: 2,
		},
		{
			name: "seedance-2.5-video-reference",
			options: VideoOptions{
				ModelID: "seedance-2.5", Prompt: "continue motion", Ratio: "9:16", Resolution: "720p", Duration: 10, Audio: true,
				ReferenceVideos: []MediaReference{{Data: []byte("mp4-data"), ContentType: "video/mp4", DurationSec: 8.25}},
			},
			wantScene: "reference", wantAIType: 14244, wantVideos: 1, wantRefBand: "6-10",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, request, uploadCount, closeServer := referenceVideoTestClient(t)
			defer closeServer()
			_, _, err := client.GenerateVideo(context.Background(), Account{Cookie: "ouss=session"}, tt.options)
			if err != nil {
				t.Fatalf("GenerateVideo() error = %v", err)
			}
			got := request()
			if got.VideoConfig.Scene != tt.wantScene || got.VideoConfig.AIType != tt.wantAIType {
				t.Fatalf("scene/aiType = %q/%d, want %q/%d", got.VideoConfig.Scene, got.VideoConfig.AIType, tt.wantScene, tt.wantAIType)
			}
			if len(got.Messages) != 1 || len(got.Messages[0].Attachments) != tt.wantImages+tt.wantVideos || uploadCount() != tt.wantImages+tt.wantVideos {
				t.Fatalf("attachment/upload counts = %d/%d", len(got.Messages[0].Attachments), uploadCount())
			}
			switch tt.wantScene {
			case "reference":
				if got.VideoConfig.Reference == nil || len(got.VideoConfig.Reference.ReferenceImages) != tt.wantImages || len(got.VideoConfig.Reference.ReferenceVideos) != tt.wantVideos || got.VideoConfig.Reference.RefDuration != tt.wantRefBand {
					t.Fatalf("reference config = %#v", got.VideoConfig.Reference)
				}
			case "frame_based":
				if got.VideoConfig.FrameBased == nil || got.VideoConfig.FrameBased.FirstFrame == "" || got.VideoConfig.FrameBased.LastFrame == "" {
					t.Fatalf("frame config = %#v", got.VideoConfig.FrameBased)
				}
			}
		})
	}
}

func TestGenerateVideoRejectsReferenceVideoForSeedance15(t *testing.T) {
	client := NewClient("")
	client.signer = stubSigner{sig: Signature{JT: "signed"}}
	_, _, err := client.GenerateVideo(context.Background(), Account{Cookie: "ouss=session"}, VideoOptions{
		ModelID: "seedance-1.5-pro", Prompt: "hello", Ratio: "16:9", Resolution: "480p", Duration: 5,
		ReferenceVideos: []MediaReference{{Data: []byte("video"), ContentType: "video/mp4", DurationSec: 5}},
	})
	if err == nil || !strings.Contains(err.Error(), "does not support reference videos") {
		t.Fatalf("GenerateVideo() error = %v", err)
	}
}

func referenceVideoTestClient(t *testing.T) (*Client, func() videoRequest, func() int, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case uploadTokenPath:
			var tokenRequest uploadTokenRequest
			if err := json.NewDecoder(r.Body).Decode(&tokenRequest); err != nil {
				t.Errorf("decode upload token request: %v", err)
			}
			keys := map[string]uploadCredential{}
			for _, file := range tokenRequest.Files {
				filename := file.Filename + "." + file.FileExt
				keys[filename] = uploadCredential{Bucket: "ot-pt", ObjectPath: "uploaded/" + filename, SessionKey: "test-token"}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"code": 0}, "data": map[string]any{"KeyList": keys}})
		default:
			http.NotFound(w, r)
		}
	}))
	uploads := 0
	client := NewClient("")
	client.baseURL = server.URL
	submitter := &stubSubmitter{
		chatID: "chat-ref",
		stream: "data: {\"event\":\"end\",\"url\":\"https://cdn.example/reference.mp4\"}\n",
	}
	client.signer = submitter
	client.directClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.Method {
		case http.MethodPost:
			return testHTTPResponse(http.StatusOK, http.Header{"Location": {"https://storage.googleapis.com/resumable/session"}}, nil), nil
		case http.MethodPut:
			uploads++
			return testHTTPResponse(http.StatusOK, nil, nil), nil
		default:
			return nil, fmt.Errorf("unexpected direct request %s", req.Method)
		}
	})}
	return client, func() videoRequest { return submitter.request(t) }, func() int { return uploads }, server.Close
}

func TestParseChatVideoReadsTerminalState(t *testing.T) {
	ready := `{"status":{"code":0},"data":{"messageList":[{"role":"user","content":"prompt"},{"role":"assistant","content":"<video src=\"https://cdn.example/ready.mp4\"></video>","data":"{\"status\":3}"}]}}`
	verdict, err := parseChatVideo([]byte(ready))
	if err != nil || verdict.VideoURL != "https://cdn.example/ready.mp4" || verdict.Pending {
		t.Fatalf("ready verdict = %+v, err = %v", verdict, err)
	}
	failed := `{"status":{"code":0},"data":{"messageList":[{"role":"assistant","content":"<p>Oreate failed to generate a video, please try again.</p>","data":"{\"status\":2}"}]}}`
	verdict, err = parseChatVideo([]byte(failed))
	if err != nil || !verdict.Failed || verdict.Pending {
		t.Fatalf("failed verdict = %+v, err = %v", verdict, err)
	}
	if got := upstreamFailureMessage(verdict.Message); got != "Oreate failed to generate a video, please try again." {
		t.Fatalf("failure message = %q", got)
	}
	generating := `{"status":{"code":0},"data":{"messageList":[{"role":"assistant","content":"generating video","data":"{\"status\":1}"}]}}`
	verdict, err = parseChatVideo([]byte(generating))
	if err != nil || !verdict.Pending || verdict.Failed || verdict.VideoURL != "" {
		t.Fatalf("pending verdict = %+v, err = %v", verdict, err)
	}
	empty := `{"status":{"code":0},"data":{"messageList":[{"role":"user","content":"prompt"}]}}`
	verdict, err = parseChatVideo([]byte(empty))
	if err != nil || !verdict.Pending || verdict.Failed || verdict.VideoURL != "" {
		t.Fatalf("empty-chat verdict = %+v, err = %v", verdict, err)
	}
}

func TestGenerateVideoReportsChatFailureVerdict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != messageListPath {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"status":{"code":0},"data":{"messageList":[{"role":"assistant","content":"<p>Oreate failed to generate a video, please try again.</p>","data":"{\"status\":2}"}]}}`)
	}))
	defer server.Close()
	client := NewClient("")
	client.baseURL = server.URL
	client.signer = &stubSubmitter{chatID: "chat-1", stream: "data: {\"event\":\"start\",\"logId\":\"555\"}\n"}
	_, _, err := client.GenerateVideo(context.Background(), Account{Cookie: "ouss=x"}, VideoOptions{
		ModelID: "seedance-2.0-mini", Prompt: "hello", Ratio: "16:9", Resolution: "480p", Duration: 5,
	})
	if err == nil || !strings.Contains(err.Error(), "failed to generate a video") {
		t.Fatalf("GenerateVideo error = %v, want the chat verdict", err)
	}
}
