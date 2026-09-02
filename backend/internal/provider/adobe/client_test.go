package adobe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCookieExchangeScopeIncludesCurrentFireflyPlatformScopes(t *testing.T) {
	if !strings.Contains(refreshURL, "jslVersion=v2-v0.54.0-3-g58cfcb7") {
		t.Fatalf("refreshURL does not use the current Firefly IMS client version: %s", refreshURL)
	}
	scopes := make(map[string]bool)
	for _, scope := range strings.Split(scopeValue, ",") {
		scopes[scope] = true
	}
	for _, required := range []string{"AdobeID", "firefly_api", "creative_production", "tk_platform", "tk_platform_sync", "profile"} {
		if !scopes[required] {
			t.Errorf("cookie exchange scope is missing %q", required)
		}
	}
}

func TestCookieExchangeFormIncludesFreshFingerprintRequestID(t *testing.T) {
	first, err := url.ParseQuery(buildCookieExchangeForm())
	if err != nil {
		t.Fatal(err)
	}
	second, err := url.ParseQuery(buildCookieExchangeForm())
	if err != nil {
		t.Fatal(err)
	}
	if first.Get("client_id") != clientID || first.Get("guest_allowed") != "true" || first.Get("scope") != scopeValue {
		t.Fatalf("unexpected cookie exchange form: %#v", first)
	}
	firstID := first.Get("fingerprint_request_id")
	secondID := second.Get("fingerprint_request_id")
	if _, err := uuid.Parse(firstID); err != nil {
		t.Fatalf("fingerprint_request_id is not a UUID: %q", firstID)
	}
	if firstID == secondID {
		t.Fatalf("fingerprint_request_id was reused: %q", firstID)
	}
}

func TestAdobeBrowserIdentityIsStableAndCurrent(t *testing.T) {
	first := currentAdobeFingerprint()
	for i := 0; i < 20; i++ {
		got := currentAdobeFingerprint()
		if got.profile.GetClientHelloStr() != first.profile.GetClientHelloStr() || got.userAgent != first.userAgent || got.secCHUA != first.secCHUA || got.platform != first.platform {
			t.Fatalf("Adobe browser identity changed between requests: first=%#v got=%#v", first, got)
		}
	}
	if !strings.Contains(first.userAgent, "Chrome/146.0.0.0") {
		t.Fatalf("Adobe User-Agent is stale: %q", first.userAgent)
	}
	if first.secCHUA != `"Not=A?Brand";v="99", "Google Chrome";v="146", "Chromium";v="146"` {
		t.Fatalf("unexpected Adobe sec-ch-ua: %q", first.secCHUA)
	}
	if first.platform != `"Windows"` {
		t.Fatalf("unexpected Adobe platform: %q", first.platform)
	}
}

func TestPartnerSubmitOmitsUnsignedARPSessionHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("sec-fetch-site"); got != "cross-site" {
			t.Errorf("sec-fetch-site = %q, want cross-site for adobe.com -> adobe.io", got)
		}
		if got := r.Header.Get("x-arp-session-id"); got != "" {
			t.Errorf("x-arp-session-id = %q, want absent without an Adobe-issued ARP feature token", got)
		}
		if got := r.Header.Get("x-nonce"); got == "" {
			t.Error("x-nonce is missing")
		}
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
	token := "e30.eyJ1c2VyX2lkIjoidXNlciJ9.signature"

	t.Run("image", func(t *testing.T) {
		_, _, err := client.submitImage(context.Background(), sess, token, "", "prompt", server.URL, map[string]any{"prompt": "prompt"})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("video", func(t *testing.T) {
		_, _, err := client.submitVideo(context.Background(), sess, token, "", server.URL, map[string]any{"prompt": "prompt"})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestPartnerSubmitIncludesAdobeARPSessionHeader(t *testing.T) {
	const arpSessionToken = "adobe-issued-arp-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-arp-session-id"); got != arpSessionToken {
			t.Errorf("x-arp-session-id = %q, want imported Adobe ARP token", got)
		}
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
	token := "e30.eyJ1c2VyX2lkIjoidXNlciJ9.signature"

	t.Run("image", func(t *testing.T) {
		_, _, err := client.submitImage(context.Background(), sess, token, arpSessionToken, "prompt", server.URL, map[string]any{"prompt": "prompt"})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("video", func(t *testing.T) {
		_, _, err := client.submitVideo(context.Background(), sess, token, arpSessionToken, server.URL, map[string]any{"prompt": "prompt"})
		if err != nil {
			t.Fatal(err)
		}
	})
}

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
			_, _, submitErr := client.submitImage(context.Background(), sess, "token", "", "prompt", server.URL, map[string]any{"prompt": "test"})
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
		_, _, submitErr := client.submitImage(context.Background(), sess, "token", "", "prompt", server.URL, map[string]any{"prompt": "test"})
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

func TestAccessErrorSeparatesRouteEntitlementFromCredentialAuth(t *testing.T) {
	entitlement := accessError(http.StatusForbidden, "", []byte(`{"error_code":"user_not_entitled"}`))
	if !errors.Is(entitlement, ErrEntitlement) || errors.Is(entitlement, ErrAuth) {
		t.Fatalf("user_not_entitled = %v, want only ErrEntitlement", entitlement)
	}
	auth := accessError(http.StatusForbidden, "invalid_token", []byte(`{"message":"forbidden"}`))
	if !errors.Is(auth, ErrAuth) || errors.Is(auth, ErrEntitlement) {
		t.Fatalf("generic 403 = %v, want only ErrAuth", auth)
	}
}
