package adobe

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/google/uuid"
)

func TestNewARPSessionTokenMatchesAdobeSherlockBaseShape(t *testing.T) {
	first := NewARPSessionToken()
	second := NewARPSessionToken()
	if first == second {
		t.Fatal("separate ARP sessions reused the same token")
	}

	raw, err := base64.StdEncoding.DecodeString(first)
	if err != nil {
		t.Fatalf("ARP token is not standard padded base64: %v", err)
	}
	var payload struct {
		SessionID string `json:"sid"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("ARP token payload is not JSON: %v", err)
	}
	parsed, err := uuid.Parse(payload.SessionID)
	if err != nil {
		t.Fatalf("ARP sid is not a UUID: %v", err)
	}
	if parsed.Version() != 4 {
		t.Fatalf("ARP sid UUID version = %d, want 4", parsed.Version())
	}
	if got, want := string(raw), `{"sid":"`+payload.SessionID+`"}`; got != want {
		t.Fatalf("ARP JSON = %q, want compact Adobe shape %q", got, want)
	}
}

func TestSubmissionARPSessionTokenUpgradesBaseAndPreservesImportedValue(t *testing.T) {
	base := NewARPSessionToken()
	first := SubmissionARPSessionToken(base)
	second := SubmissionARPSessionToken(base)
	if first == base || first == second {
		t.Fatal("sid-only ARP was not upgraded to a fresh submission token")
	}
	raw, err := base64.StdEncoding.DecodeString(first)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]string
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(payload["sid"]); err != nil {
		t.Fatalf("submission sid is invalid: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}_[0-9]{13}_[0-9]+_dUAL43-mnts-ants-d4_31ck__tt$`).MatchString(payload["ftr"]) {
		t.Fatalf("unexpected submission ftr shape: %q", payload["ftr"])
	}
	const imported = "opaque-adobe-issued-arp"
	if got := SubmissionARPSessionToken(imported); got != imported {
		t.Fatalf("imported ARP changed: %q", got)
	}
}
