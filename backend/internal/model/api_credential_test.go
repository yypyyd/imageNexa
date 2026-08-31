package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAPICredentialJSONNeverExposesHash(t *testing.T) {
	credential := APICredential{
		ID:         "cred-test",
		Name:       "production",
		KeyPreview: "sk-test…last",
		KeyHash:    "sha256:must-not-be-returned",
		Status:     APICredentialStatusActive,
	}
	raw, err := json.Marshal(credential)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	serialized := string(raw)
	if strings.Contains(serialized, credential.KeyHash) || strings.Contains(serialized, "key_hash") {
		t.Fatalf("credential JSON exposes hash: %s", serialized)
	}
	if !strings.Contains(serialized, credential.KeyPreview) {
		t.Fatalf("credential JSON omitted preview: %s", serialized)
	}
}
