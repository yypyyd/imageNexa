package service

import (
	"strings"
	"testing"
)

func TestGeneratePlainAPIKeyUsesOpenAIShape(t *testing.T) {
	plain, err := generatePlainAPIKey()
	if err != nil {
		t.Fatalf("generatePlainAPIKey() error = %v", err)
	}
	if !strings.HasPrefix(plain, "sk-") || !validPlainAPIKey(plain) {
		t.Fatalf("generated key has invalid OpenAI shape: %q", plain)
	}
	if preview := previewAPIKey(plain); strings.Contains(preview, plain) || len(preview) >= len(plain) {
		t.Fatalf("preview leaks plaintext: plain=%q preview=%q", plain, preview)
	}
}

func TestValidPlainAPIKeyRejectsAlternateShapes(t *testing.T) {
	for _, invalid := range []string{"", "abc", "api-1234567890123456", "sk-space value", "sk-短密钥"} {
		if validPlainAPIKey(invalid) {
			t.Fatalf("validPlainAPIKey(%q) = true", invalid)
		}
	}
}

func TestRandomSecretIsUniqueAndURLSafe(t *testing.T) {
	first, err := randomSecret(32)
	if err != nil {
		t.Fatalf("randomSecret() error = %v", err)
	}
	second, err := randomSecret(32)
	if err != nil {
		t.Fatalf("randomSecret() second error = %v", err)
	}
	if first == second || len(first) < 40 || strings.ContainsAny(first, "+/=") {
		t.Fatalf("randomSecret produced unsafe output: %q / %q", first, second)
	}
}
