package service

import "testing"

func TestNormalizeAdminPublicOrigin(t *testing.T) {
	for _, raw := range []string{
		"https://",
		"https://user:secret@example.com",
		"https://example.com/api",
		"https://example.com?next=internal",
		"https://example.com#fragment",
		"javascript://example.com",
	} {
		if _, err := normalizeAdminPublicOrigin(raw, false); err == nil {
			t.Fatalf("normalizeAdminPublicOrigin(%q) unexpectedly succeeded", raw)
		}
	}
	if got, err := normalizeAdminPublicOrigin("https://api.example.com/", true); err != nil || got != "https://api.example.com" {
		t.Fatalf("production origin = %q, %v", got, err)
	}
	for _, raw := range []string{"http://api.example.com", "https://localhost", "https://127.0.0.1:6061"} {
		if _, err := normalizeAdminPublicOrigin(raw, true); err == nil {
			t.Fatalf("production origin %q unexpectedly succeeded", raw)
		}
	}
}

func TestNormalizeOutboundProxyRequiresParsedSchemeAndHost(t *testing.T) {
	for _, raw := range []string{
		"https://",
		"https://proxy.example.com.evil.test/?target=proxy.example.com",
		"httpx://proxy.example.com",
		"socks5://",
	} {
		if _, err := normalizeOutboundProxy(raw); err == nil {
			t.Fatalf("normalizeOutboundProxy(%q) unexpectedly succeeded", raw)
		}
	}
	if got, err := normalizeOutboundProxy("socks5://user:pass@proxy.example.com:1080"); err != nil || got == "" {
		t.Fatalf("authenticated proxy = %q, %v", got, err)
	}
}
