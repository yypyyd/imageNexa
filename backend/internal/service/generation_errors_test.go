package service

import (
	"errors"
	"strings"
	"testing"

	"backend/internal/provider/custom"
)

func TestGenerationErrorSanitizationNeverReturnsProviderText(t *testing.T) {
	secret := "Authorization: Bearer sk-secret Cookie=sessionid-secret https://user:pass@example.test/private"
	err := errors.Join(custom.ErrTemporaryUpstream, errors.New(secret))
	got := safeGenerationErrorText(err)
	if got != ErrProviderTemporary.Error() {
		t.Fatalf("safe error = %q", got)
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "example.test") {
		t.Fatalf("safe error leaked provider text: %q", got)
	}
	if legacy := safeStoredGenerationError(secret); legacy != ErrProviderExecution.Error() {
		t.Fatalf("legacy stored error = %q", legacy)
	}
}

func TestProviderSnapshotScopeSeparatesMediaFromText(t *testing.T) {
	if bucket, unit, ok := providerSnapshotScope("chatgpt"); !ok || bucket != "chatgpt.image" || unit != "generations" {
		t.Fatalf("chatgpt scope = %q %q %v", bucket, unit, ok)
	}
	textRoute := canonicalRouteForTest(t, "text.gpt-5-5-mini.chatgpt")
	if textRoute.QuotaBucketKey == "chatgpt.image" {
		t.Fatal("chatgpt image quota bucket is shared with text")
	}
	if bucket, _, _ := providerSnapshotScope(textRoute.Provider); bucket == textRoute.QuotaBucketKey {
		t.Fatal("image quota probe was treated as authoritative for text")
	}
}
