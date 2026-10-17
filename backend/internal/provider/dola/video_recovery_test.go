package dola

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestVideoAcceptedCallbackRunsBeforePolling(t *testing.T) {
	var submits atomic.Int32
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case completionPath:
			submits.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: SSE_ACK\ndata: {\"ack_client_meta\":{\"conversation_id\":\"conversation-1\"}}\n\n"))
		case chainPath:
			polls.Add(1)
			http.Error(w, "unexpected poll", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient("")
	client.baseURL = server.URL
	callbackErr := errors.New("durable write unavailable")
	called := false
	_, _, err := client.GenerateVideo(context.Background(), Account{ID: "account-1", Cookie: "sessionid=test"}, VideoOptions{
		ModelID: DefaultVideoModel, Prompt: "test", Ratio: "16:9", Duration: 30,
		OnAccepted: func(id string) error {
			called = true
			if id != "conversation-1" {
				t.Fatalf("unexpected conversation id %q", id)
			}
			return callbackErr
		},
	})
	var accepted *VideoSubmissionError
	if !called || !errors.Is(err, ErrTaskAccepted) || !errors.As(err, &accepted) || accepted.ConversationID != "conversation-1" {
		t.Fatalf("callback=%v accepted=%#v err=%v", called, accepted, err)
	}
	if submits.Load() != 1 || polls.Load() != 0 {
		t.Fatalf("submits=%d polls=%d", submits.Load(), polls.Load())
	}
}

func TestResumeVideoTaskNeverResubmitsPrompt(t *testing.T) {
	var submits atomic.Int32
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == completionPath {
			submits.Add(1)
			http.Error(w, "unsafe resubmit", http.StatusInternalServerError)
			return
		}
		if r.URL.Path != chainPath {
			http.NotFound(w, r)
			return
		}
		polls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"downlink_body":{"pull_singe_chain_downlink_body":{"messages":[{"user_type":2,"status":0,"ext":{"has_video_gen":"1"},"content_block":[{"block_type":2074,"content":{"creation_block":{"creations":[{"type":2,"video":{"download_url":"http://media.example/video.mp4","duration":30}}]}}}] }]}}}}`))
	}))
	defer server.Close()

	client := NewClient("")
	client.baseURL = server.URL
	meta, err := client.ResumeVideoTask(context.Background(), Account{ID: "account-1", Cookie: "sessionid=test"}, "conversation-1")
	if err != nil || meta["video_url"] != "https://media.example/video.mp4" {
		t.Fatalf("meta=%#v err=%v", meta, err)
	}
	if submits.Load() != 0 || polls.Load() != 1 {
		t.Fatalf("submits=%d polls=%d", submits.Load(), polls.Load())
	}
}

func TestVideoFinalFailureEndsInitialAndRecoveryPolling(t *testing.T) {
	for _, resume := range []bool{false, true} {
		name := "initial"
		if resume {
			name = "recovery"
		}
		t.Run(name, func(t *testing.T) {
			var submits, polls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case completionPath:
					submits.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("event: SSE_ACK\ndata: {\"ack_client_meta\":{\"conversation_id\":\"conversation-1\"}}\n\n"))
				case chainPath:
					polls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"code":0,"data":{"downlink_body":{"pull_singe_chain_downlink_body":{"messages":[{"user_type":2,"status":0,"content":"[{\"block_type\":10000,\"content\":{\"text_block\":{\"text\":\"Something went wrong. Please try again.\"}}},{\"block_type\":10000,\"content\":{\"text_block\":{\"text\":\"\"}}}]"}]}}}}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := NewClient("")
			client.baseURL = server.URL
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			account := Account{ID: "account-1", Cookie: "sessionid=test"}
			if resume {
				_, err := client.ResumeVideoTask(ctx, account, "conversation-1")
				if !errors.Is(err, ErrVideoFailed) || submits.Load() != 0 {
					t.Fatalf("err=%v submits=%d", err, submits.Load())
				}
			} else {
				_, _, err := client.GenerateVideo(ctx, account, VideoOptions{
					ModelID: DefaultVideoModel, Prompt: "test", Ratio: "16:9", Duration: 30,
				})
				var accepted *VideoSubmissionError
				if !errors.Is(err, ErrTaskAccepted) || !errors.As(err, &accepted) ||
					accepted.ConversationID != "conversation-1" || !errors.Is(accepted.Cause, ErrVideoFailed) || submits.Load() != 1 {
					t.Fatalf("err=%v accepted=%#v submits=%d", err, accepted, submits.Load())
				}
			}
			if polls.Load() != 1 {
				t.Fatalf("terminal failure polled %d times", polls.Load())
			}
		})
	}
}

func TestVideoFailureRequiresFinalAssistantReply(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
		failed  bool
	}{
		{"finished assistant", `{"user_type":2,"status":0,"content_block":[{"block_type":10000,"content":{"text_block":{"text":"Something went wrong. Please try again."}}}]}`, true},
		{"normalized whitespace", `{"user_type":2,"status":0,"content_block":[{"block_type":10000,"content":{"text_block":{"text":"  SOMETHING went wrong.\nPlease try again.  "}}}]}`, true},
		{"failed after starting", `{"user_type":2,"status":0,"ext":{"has_video_gen":"1"},"content_block":[{"block_type":10000,"content":{"text_block":{"text":"Something went wrong. Please try again."}}}]}`, true},
		{"user prompt", `{"user_type":1,"status":0,"content_block":[{"block_type":10000,"content":{"text_block":{"text":"Something went wrong. Please try again."}}}]}`, false},
		{"unfinished assistant", `{"user_type":2,"status":1,"content_block":[{"block_type":10000,"content":{"text_block":{"text":"Something went wrong. Please try again."}}}]}`, false},
		{"missing status", `{"user_type":2,"content_block":[{"block_type":10000,"content":{"text_block":{"text":"Something went wrong. Please try again."}}}]}`, false},
		{"still rendering", `{"user_type":2,"status":0,"ext":{"has_video_gen":"1"},"content_block":[{"block_type":10000,"content":{"text_block":{"text":"Your video is generating. Please wait."}}}]}`, false},
		{"quoted failure", `{"user_type":2,"status":0,"content_block":[{"block_type":10000,"content":{"text_block":{"text":"The scene says: Something went wrong. Please try again."}}}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"code":0,"downlink_body":{"pull_singe_chain_downlink_body":{"messages":[` + tc.message + `]}}}`))
			}))
			defer server.Close()
			client := NewClient("")
			client.baseURL = server.URL
			_, verdict, err := client.chainOnce(context.Background(), Account{Cookie: "sessionid=test"}, "conversation-1", server.URL, "tab")
			if err != nil || verdict.generationFailed != tc.failed {
				t.Fatalf("verdict=%#v err=%v", verdict, err)
			}
		})
	}
}

func TestVideoArtifactTakesPrecedenceOverFailureText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"downlink_body":{"pull_singe_chain_downlink_body":{"messages":[{"user_type":2,"status":0,"content_block":[{"block_type":10000,"content":{"text_block":{"text":"Something went wrong. Please try again."}}},{"block_type":2074,"content":{"creation_block":{"creations":[{"type":2,"video":{"download_url":"https://media.example/video.mp4","duration":30}}]}}}]}]}}}`))
	}))
	defer server.Close()
	client := NewClient("")
	client.baseURL = server.URL
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	meta, err := client.ResumeVideoTask(ctx, Account{Cookie: "sessionid=test"}, "conversation-1")
	if err != nil || meta["video_url"] != "https://media.example/video.mp4" {
		t.Fatalf("meta=%#v err=%v", meta, err)
	}
}

func TestVideoPlanningFailureRequiresCompletedUnstartedPlan(t *testing.T) {
	for _, tc := range []struct {
		name, toolList, taskID, hasVideo string
		role, status, code, fc           int
		want                             bool
	}{
		{name: "failed plan", role: 2, code: 1, toolList: "[]", want: true},
		{name: "successful plan", role: 2, toolList: "[]"},
		{name: "unfinished", role: 2, status: 1, code: 1, toolList: "[]"},
		{name: "user message", role: 1, code: 1, toolList: "[]"},
		{name: "tool ran", role: 2, code: 1, fc: 100, toolList: "[]"},
		{name: "task created", role: 2, code: 1, taskID: "task-1", toolList: "[]"},
		{name: "video started", role: 2, code: 1, hasVideo: "1", toolList: "[]"},
		{name: "tool list present", role: 2, code: 1, toolList: `[{"task_id":123,"status":1}]`},
		{name: "missing tool list", role: 2, code: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stages, _ := json.Marshal([]any{map[string]any{
				"type": 1, "task_id": tc.taskID, "error_code": tc.code,
				"duration_info": []any{map[string]any{"step_name": "plan", "duration": 8512}, map[string]any{"step_name": "fc", "duration": tc.fc}},
			}})
			message := map[string]any{"user_type": tc.role, "status": tc.status, "ext": map[string]string{
				"auto_create_creation_asset": "0", "has_video_gen": tc.hasVideo,
				"ai_creation_tool_list": tc.toolList, "ai_creation_duration_info": string(stages),
			}, "content_block": []any{map[string]any{"block_type": 10000, "content": map[string]any{"text_block": map[string]any{"text": "發生錯誤，請稍後再試。"}}}}}
			data, _ := json.Marshal(map[string]any{"code": 0, "downlink_body": map[string]any{"pull_singe_chain_downlink_body": map[string]any{"messages": []any{message}}}})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) }))
			defer server.Close()
			client := NewClient("")
			client.baseURL = server.URL
			account := Account{Cookie: "sessionid=test"}
			_, verdict, err := client.chainOnce(context.Background(), account, "conversation-1", server.URL, "tab")
			if err != nil || verdict.planningFailed != tc.want {
				t.Fatalf("verdict=%#v err=%v", verdict, err)
			}
			if tc.want {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_, err := client.ResumeVideoTask(ctx, account, "conversation-1")
				if !errors.Is(err, ErrVideoPlanningFailed) || !errors.Is(err, ErrVideoFailed) {
					t.Fatalf("error=%v", err)
				}
			}
		})
	}
}

func TestVideoPolicyResultTerminatesWithoutTextMatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"downlink_body":{"pull_singe_chain_downlink_body":{"messages":[{"user_type":2,"status":0,"ext":{"ai_creation_res_code":"710082022"}}]}}}`))
	}))
	defer server.Close()
	client := NewClient("")
	client.baseURL = server.URL
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := client.ResumeVideoTask(ctx, Account{Cookie: "sessionid=test"}, "conversation-1")
	if !errors.Is(err, ErrContentRejected) {
		t.Fatalf("error=%v", err)
	}
}

func TestVideoBodyPreservesVerified30SecondInstruction(t *testing.T) {
	prompt := "雨后的湖面，缓慢推进镜头"
	body := buildVideoBody(DefaultVideoModel, prompt, "16:9", 30, "", nil)
	messages := body["messages"].([]map[string]any)
	text := messages[0]["content_block"].([]map[string]any)[0]["content"].(map[string]any)["text_block"].(map[string]any)["text"]
	if text != "Generated video: "+prompt+", 16:9" {
		t.Fatalf("unexpected instruction: %v", text)
	}
	ability := body["chat_ability"].(map[string]any)
	var param struct {
		Model    string `json:"model"`
		Ratio    string `json:"ratio"`
		Duration int    `json:"duration"`
		Input    struct {
			Prompt string `json:"user_input_content"`
			Format string `json:"reply_message_format"`
		} `json:"input_box_content"`
	}
	if err := json.Unmarshal([]byte(ability["ability_param"].(string)), &param); err != nil {
		t.Fatal(err)
	}
	if ability["ability_type"] != 17 || param.Model != DefaultVideoModel || param.Ratio != "16:9" || param.Duration != 30 || param.Input.Prompt != prompt || param.Input.Format != "Generated video: %s" {
		t.Fatalf("unexpected ability: %#v", param)
	}
}
