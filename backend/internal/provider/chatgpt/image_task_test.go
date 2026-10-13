package chatgpt

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	http "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
)

type imageTestClient struct {
	tlsclient.HttpClient
	response *http.Response
	err      error
	calls    int
}

func (c *imageTestClient) Do(*http.Request) (*http.Response, error) {
	c.calls++
	return c.response, c.err
}

func TestImageSubmitDurablyAcknowledgedBeforePolling(t *testing.T) {
	const id = "12345678-1234-1234-1234-123456789abc"
	client := &imageTestClient{response: &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("data: {\"conversation_id\":\"" + id + "\",\"image_gen_async\":true}\n\ndata: [DONE]\n\n"))}}
	called := false
	got, _, _, err := NewClient("").startImageGeneration(context.Background(), client, "test-token", "test", &chatRequirements{}, "", "gpt-image-2", nil, func(value string) error {
		called = true
		if value != id {
			t.Fatal("wrong handle")
		}
		return nil
	})
	if err != nil || !called || got != id || client.calls != 1 {
		t.Fatalf("accepted=%v id=%q calls=%d error=%v", called, got, client.calls, err)
	}
}

func TestImageSubmitCallbackFailureCannotResubmit(t *testing.T) {
	const id = "12345678-1234-1234-1234-123456789abc"
	client := &imageTestClient{response: &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("data: {\"conversation_id\":\"" + id + "\"}\n\n"))}}
	_, _, _, err := NewClient("").startImageGeneration(context.Background(), client, "test-token", "test", &chatRequirements{}, "", "gpt-image-2", nil, func(string) error { return errors.New("database unavailable") })
	if !errors.Is(err, ErrTaskAccepted) || AcceptedTaskID(err) != id || errors.Is(err, ErrTemporaryUpstream) {
		t.Fatalf("unsafe submission error: %v", err)
	}
}

func TestLostImageSubmitResponseIsAmbiguous(t *testing.T) {
	for _, test := range []struct {
		name   string
		client *imageTestClient
	}{
		{"transport", &imageTestClient{err: io.ErrUnexpectedEOF}},
		{"server", &imageTestClient{response: &http.Response{StatusCode: 502, Body: io.NopCloser(strings.NewReader("gateway failed"))}}},
		{"missing handle", &imageTestClient{response: &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, _, err := NewClient("").startImageGeneration(context.Background(), test.client, "test-token", "test", &chatRequirements{}, "", "gpt-image-2", nil)
			if !errors.Is(err, ErrTaskSubmissionUnknown) || errors.Is(err, ErrTemporaryUpstream) || test.client.calls != 1 {
				t.Fatalf("unsafe retry signal: %v", err)
			}
		})
	}
}

func TestAcceptedImageFailureDoesNotExposeCredentialsOrAuthRetry(t *testing.T) {
	err := AcceptedFailure("existing-task", errors.Join(ErrAuth, errors.New("secret-token")))
	if !errors.Is(err, ErrTaskAccepted) || errors.Is(err, ErrAuth) || strings.Contains(err.Error(), "secret-token") {
		t.Fatal("accepted wrapper leaked cause or allowed auth retry")
	}
	if !errors.Is(AcceptedFailure("existing-task", ErrContentPolicy), ErrContentPolicy) {
		t.Fatal("terminal rejection must remain identifiable")
	}
}

func TestRecoveredImageExcludesEchoedReference(t *testing.T) {
	ref := "file_00000000aaaaaaaaaaaaaaaaaaaaaaaa"
	output := "file_00000000bbbbbbbbbbbbbbbbbbbbbbbb"
	node := func(role, content string) any {
		return map[string]any{"message": map[string]any{"author": map[string]any{"role": role}, "content": content}}
	}
	conversation := map[string]any{"mapping": map[string]any{"user": node("user", ref), "tool": node("tool", ref+" "+output)}}
	files, _ := extractImageIDs(conversation)
	files = dropIDs(files, conversationReferenceIDs(conversation))
	if len(files) != 1 || files[0] != output {
		t.Fatalf("reference returned as generated image: %v", files)
	}
}
