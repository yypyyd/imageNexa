package dola

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
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
