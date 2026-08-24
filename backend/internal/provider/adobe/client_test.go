package adobe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSubmitImageHasNoGlobalGateOrPacing(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		w.Header().Set("x-override-status-link", "https://poll.example/result")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := NewClient("test", "")
	sess, err := client.newSubmitTLSClient()
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, submitErr := client.submitImage(context.Background(), sess, "token", "prompt", server.URL, map[string]any{"prompt": "test"})
			errs <- submitErr
		}()
	}

	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-timer.C:
			close(release)
			t.Fatal("concurrent Adobe submit was blocked by a global gate or pacing delay")
		}
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestSystemUnderLoadRemainsTemporaryWithoutBreaker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error_code":"timeout_error","message":"system under load"}`))
	}))
	defer server.Close()

	client := NewClient("test", "")
	sess, err := client.newSubmitTLSClient()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		_, _, submitErr := client.submitImage(context.Background(), sess, "token", "prompt", server.URL, map[string]any{"prompt": "test"})
		if !errors.Is(submitErr, ErrTemporaryUpstream) {
			t.Fatalf("attempt %d error = %v, want ErrTemporaryUpstream", i+1, submitErr)
		}
		if strings.Contains(strings.ToLower(submitErr.Error()), "熔断") {
			t.Fatalf("attempt %d returned breaker state: %v", i+1, submitErr)
		}
	}
}

func TestAdobeProxyClientsUseConfiguredProxy(t *testing.T) {
	// A malformed proxy makes the boundary observable without network traffic.
	client := NewClient("test", "http://%zz")
	if _, err := client.newDirectTLSClient(); err != nil {
		t.Fatalf("direct non-submit session unexpectedly used configured proxy: %v", err)
	}
	if _, err := client.newProxyTLSClient(); err == nil {
		t.Fatal("proxy session accepted malformed configured proxy")
	}
	if _, err := client.newSubmitTLSClient(); err == nil {
		t.Fatal("submit session accepted malformed configured proxy")
	}
}

func TestContentRejectionClassification(t *testing.T) {
	privacyBody := `{"error_code":"reference_image_privacy_error","message":"The reference image contains a real person's face and cannot be used to generate content."}`
	if contentRejectionError(451, privacyBody) == nil {
		t.Fatal("reference image privacy refusal should be classified as content rejection")
	}
	err := contentRejectionError(451, privacyBody)
	if !errors.Is(err, ErrContentRejected) || !strings.Contains(err.Error(), "真人面部") {
		t.Fatalf("privacy refusal = %v, want friendly ErrContentRejected", err)
	}
	if contentRejectionError(451, `{"error_code":"image_unsafe"}`) == nil {
		t.Fatal("image_unsafe should be classified as content rejection")
	}
	imageErr := contentRejectionError(451, `{"error_code":"image_unsafe"}`)
	var imageRejection *ContentRejectionError
	if !errors.As(imageErr, &imageRejection) || imageRejection.Code != "image_unsafe" || !isRetryableGeneratedImageRejection(imageErr) {
		t.Fatalf("image refusal = %#v, want retryable typed image_unsafe", imageErr)
	}
	promptErr := contentRejectionError(451, `{"error":{"error_code":"prompt_unsafe"}}`)
	var promptRejection *ContentRejectionError
	if !errors.As(promptErr, &promptRejection) || promptRejection.Code != "prompt_unsafe" || isRetryableGeneratedImageRejection(promptErr) {
		t.Fatalf("prompt refusal = %#v, want non-retryable typed prompt_unsafe", promptErr)
	}
	if isRetryableGeneratedImageRejection(err) {
		t.Fatal("reference image privacy refusal must not retry")
	}
	if contentRejectionError(451, `{"error_code":"legal_error","message":"{}"}`) != nil {
		t.Fatal("generic legal_error must remain an upstream/legal failure")
	}
	if contentRejectionError(500, privacyBody) != nil {
		t.Fatal("non-451 response must not be classified as a content rejection")
	}
}
