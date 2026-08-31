package service

import (
	"testing"

	"backend/internal/model"
)

func TestCanonicalImageFingerprintTreatsDefaultURLAsExplicitURL(t *testing.T) {
	base := V1ImageRequest{
		Model: "gpt-image-2", Prompt: "draw a fox", N: 1,
		Size: "1024x1024", ReferenceImages: []string{"data:image/png;base64,AAAA"},
	}
	omitted, omittedFingerprint, err := canonicalImageIdempotencyInput(base)
	if err != nil {
		t.Fatalf("canonicalImageIdempotencyInput() error = %v", err)
	}
	explicit := base
	explicit.ResponseFormat = "url"
	_, explicitFingerprint, err := canonicalImageIdempotencyInput(explicit)
	if err != nil {
		t.Fatalf("canonicalImageIdempotencyInput(url) error = %v", err)
	}
	if omitted.ResponseFormat != "url" || omittedFingerprint != explicitFingerprint {
		t.Fatalf("default URL fingerprint %q != explicit URL fingerprint %q", omittedFingerprint, explicitFingerprint)
	}
}

func TestEventOwnershipFailsClosedWithoutPrincipal(t *testing.T) {
	event := &model.EventLog{Kind: "video", APICredentialID: "cred-owner"}
	if eventOwnedByPrincipal(event, nil, "video") {
		t.Fatal("nil principal was allowed to read a video")
	}
	if eventOwnedByPrincipal(event, &APIPrincipal{}, "video") {
		t.Fatal("principal without credential was allowed to read a video")
	}
	if eventOwnedByPrincipal(event, &APIPrincipal{Credential: &model.APICredential{ID: "cred-other"}}, "video") {
		t.Fatal("different credential was allowed to read a video")
	}
	if !eventOwnedByPrincipal(event, &APIPrincipal{Credential: &model.APICredential{ID: "cred-owner"}}, "video") {
		t.Fatal("owner credential was rejected")
	}
}

func TestCanonicalImageFingerprintHashesPreGridReferencesAndGridIntent(t *testing.T) {
	in := V1ImageRequest{
		Model: "nano-banana-2", Prompt: "portrait", RequestID: "same-key",
		ReferenceImages: []string{"data:image/png;base64,first", "data:image/png;base64,second"},
		ReferenceGrid:   true,
	}
	canonical, fingerprint, err := canonicalImageIdempotencyInput(in)
	if err != nil {
		t.Fatalf("canonicalImageIdempotencyInput() error = %v", err)
	}
	if fingerprint != imageRequestFingerprint(canonical) {
		t.Fatal("canonical fingerprint was not based on the unmodified input references")
	}
	withoutGrid := in
	withoutGrid.ReferenceGrid = false
	_, otherFingerprint, err := canonicalImageIdempotencyInput(withoutGrid)
	if err != nil {
		t.Fatalf("canonicalImageIdempotencyInput(no grid) error = %v", err)
	}
	if otherFingerprint == fingerprint {
		t.Fatal("different reference_grid behavior reused the same idempotency fingerprint")
	}
}
