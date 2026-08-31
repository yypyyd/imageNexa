package service

import (
	"context"
	"errors"
	"testing"

	"backend/internal/netguard"
)

func TestNormalizeCustomBaseURLRejectsUnsafeUpstreams(t *testing.T) {
	for _, raw := range []string{
		"http://1.1.1.1/v1",
		"https://user:secret@1.1.1.1/v1",
		"https://1.1.1.1:8443/v1",
		"https://localhost/v1",
		"https://127.0.0.1/v1",
		"https://169.254.169.254/latest/meta-data",
		"https://10.0.0.8/v1",
		"https://1.1.1.1/v1?target=internal",
		"https://1.1.1.1/v1#fragment",
	} {
		if _, err := normalizeCustomBaseURL(context.Background(), raw); !errors.Is(err, netguard.ErrUnsafeAssetURL) {
			t.Fatalf("normalizeCustomBaseURL(%q) error = %v, want ErrUnsafeAssetURL", raw, err)
		}
	}
}

func TestNormalizeCustomBaseURLKeepsPublicHTTPSPath(t *testing.T) {
	got, err := normalizeCustomBaseURL(context.Background(), " https://1.1.1.1/v1/ ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://1.1.1.1/v1" {
		t.Fatalf("normalizeCustomBaseURL() = %q", got)
	}
}
