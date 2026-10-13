package grok

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	http "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
)

type videoTestClient struct {
	tlsclient.HttpClient
	response *http.Response
	err      error
	calls    int
}

func (c *videoTestClient) Do(*http.Request) (*http.Response, error) {
	c.calls++
	return c.response, c.err
}

type callbackCheckedBody struct {
	reader *strings.Reader
	called *bool
}

func (b *callbackCheckedBody) Read(p []byte) (int, error) {
	if !*b.called {
		return 0, errors.New("response body read before accepted callback")
	}
	return b.reader.Read(p)
}

func (b *callbackCheckedBody) Close() error { return nil }

func TestVideoSubmitPersistsHandleBeforeReadingAcceptedStream(t *testing.T) {
	called := false
	client := &videoTestClient{response: &http.Response{StatusCode: http.StatusOK}}
	client.response.Body = &callbackCheckedBody{reader: strings.NewReader("stream"), called: &called}
	taskURL := "https://assets.grok.com/users/u/generated/p/generated_video.mp4"
	body, err := NewClient("").postStreamAccepted(context.Background(), client, "token", "/rest/app-chat/conversations/new", map[string]any{"test": true}, taskURL, func(got string) error {
		called = true
		if got != taskURL {
			t.Fatalf("unexpected task URL %q", got)
		}
		return nil
	})
	if err != nil || body != "stream" || !called || client.calls != 1 {
		t.Fatalf("body=%q called=%v calls=%d err=%v", body, called, client.calls, err)
	}
}

func TestVideoSubmitDoesNotAcceptExplicitHTTPRejection(t *testing.T) {
	called := false
	client := &videoTestClient{response: &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader("rejected"))}}
	_, status, err := NewClient("").doPostAccepted(context.Background(), client, "token", "/rest/app-chat/conversations/new", nil, func() error {
		called = true
		return nil
	})
	if err != nil || status != http.StatusForbidden || called {
		t.Fatalf("status=%d called=%v err=%v", status, called, err)
	}
}

func TestAmbiguousVideoTransportPersistsHandleWithoutFailover(t *testing.T) {
	client := &videoTestClient{err: io.ErrUnexpectedEOF}
	taskURL := "https://assets.grok.com/users/u/generated/p/generated_video.mp4"
	callbacks := 0
	_, err := NewClient("").postStreamAccepted(context.Background(), client, "token", "/rest/app-chat/conversations/new", nil, taskURL, func(string) error {
		callbacks++
		return nil
	})
	if callbacks != 1 || !errors.Is(err, ErrTaskAccepted) || AcceptedTaskURL(err) != taskURL || errors.Is(err, ErrTemporaryUpstream) {
		t.Fatalf("callbacks=%d handle=%q err=%v", callbacks, AcceptedTaskURL(err), err)
	}
}

func TestAcceptedVideoErrorDoesNotLeakCause(t *testing.T) {
	err := AcceptedFailure("https://assets.grok.com/users/u/generated/p/generated_video.mp4", errors.Join(ErrAuth, errors.New("secret-token")))
	if !errors.Is(err, ErrTaskAccepted) || errors.Is(err, ErrAuth) || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("unsafe accepted error: %v", err)
	}
}
