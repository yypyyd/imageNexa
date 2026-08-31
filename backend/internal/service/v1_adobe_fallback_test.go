package service

import (
	"errors"
	"reflect"
	"testing"

	"backend/internal/model"
	"backend/internal/provider/adobe"
)

func TestRunAdobeImageFallbackTemporarySuccess(t *testing.T) {
	item := &model.ModelConfig{ID: "firefly-nano-banana-2", Provider: "adobe", MaxReferenceImages: 6}
	in := V1ImageRequest{Prompt: "test", ReferenceImages: []string{"data:image/png;base64,AA=="}}
	wantData := []byte("image")
	wantURL := "https://example.test/image.png"
	primaryCalls, fallbackCalls := 0, 0

	data, imageURL, err := runAdobeImageFallback(item, in,
		func(gotItem *model.ModelConfig, gotIn V1ImageRequest) ([]byte, string, error) {
			primaryCalls++
			return nil, "", adobe.ErrTemporaryUpstream
		},
		func(gotItem *model.ModelConfig, gotIn V1ImageRequest) ([]byte, string, error) {
			fallbackCalls++
			if gotItem.ID != "nano-banana-2" || gotItem.Provider != "runway" {
				t.Fatalf("unexpected fallback target: provider=%q model=%q", gotItem.Provider, gotItem.ID)
			}
			if !reflect.DeepEqual(gotIn.ReferenceImages, in.ReferenceImages) {
				t.Fatalf("reference images changed or were dropped: %#v", gotIn.ReferenceImages)
			}
			return wantData, wantURL, nil
		},
	)
	if err != nil {
		t.Fatalf("fallback failed: %v", err)
	}
	if !reflect.DeepEqual(data, wantData) || imageURL != wantURL {
		t.Fatalf("unexpected result: data=%q url=%q", data, imageURL)
	}
	if primaryCalls != 1 || fallbackCalls != 1 {
		t.Fatalf("unexpected call counts: primary=%d fallback=%d", primaryCalls, fallbackCalls)
	}
	if item.ID != "firefly-nano-banana-2" || item.Provider != "adobe" {
		t.Fatalf("original model was mutated: %#v", item)
	}
}

func TestRunAdobeImageFallbackNonTemporaryDoesNotFallback(t *testing.T) {
	fallbackCalls := 0
	wantErr := errors.New("bad request")
	_, _, err := runAdobeImageFallback(
		&model.ModelConfig{ID: "firefly-nano-banana-2", Provider: "adobe"},
		V1ImageRequest{},
		func(*model.ModelConfig, V1ImageRequest) ([]byte, string, error) { return nil, "", wantErr },
		func(*model.ModelConfig, V1ImageRequest) ([]byte, string, error) {
			fallbackCalls++
			return nil, "", nil
		},
	)
	if !errors.Is(err, wantErr) || fallbackCalls != 0 {
		t.Fatalf("non-temporary error triggered fallback: err=%v calls=%d", err, fallbackCalls)
	}
}

func TestRunAdobeImageFallbackPinnedAccountDoesNotFallback(t *testing.T) {
	fallbackCalls := 0
	_, _, err := runAdobeImageFallback(
		&model.ModelConfig{ID: "firefly-nano-banana-2", Provider: "adobe"},
		V1ImageRequest{AccountID: "fixed-adobe-account"},
		func(*model.ModelConfig, V1ImageRequest) ([]byte, string, error) {
			return nil, "", adobe.ErrTemporaryUpstream
		},
		func(*model.ModelConfig, V1ImageRequest) ([]byte, string, error) {
			fallbackCalls++
			return nil, "", nil
		},
	)
	if !errors.Is(err, adobe.ErrTemporaryUpstream) || fallbackCalls != 0 {
		t.Fatalf("pinned account triggered fallback: err=%v calls=%d", err, fallbackCalls)
	}
}

func TestRunAdobeImageFallbackFailureKeepsTemporaryClassification(t *testing.T) {
	fallbackErr := errors.New("backup pool unavailable")
	var targets []string
	_, _, err := runAdobeImageFallback(
		&model.ModelConfig{ID: "firefly-gpt-image-2", Provider: "adobe"},
		V1ImageRequest{},
		func(*model.ModelConfig, V1ImageRequest) ([]byte, string, error) {
			return nil, "", adobe.ErrTemporaryUpstream
		},
		func(gotItem *model.ModelConfig, _ V1ImageRequest) ([]byte, string, error) {
			targets = append(targets, gotItem.Provider+":"+gotItem.ID)
			return nil, "", fallbackErr
		},
	)
	if !errors.Is(err, adobe.ErrTemporaryUpstream) {
		t.Fatalf("fallback failure lost Adobe temporary classification: %v", err)
	}
	want := []string{"chatgpt:gpt-image-2", "grok:grok-imagine-image"}
	if !reflect.DeepEqual(targets, want) {
		t.Fatalf("unexpected GPT fallback chain: got=%v want=%v", targets, want)
	}
}

func TestAdobeImageFallbackExcludesNativeFirefly(t *testing.T) {
	for _, modelID := range []string{"firefly-image-5", "unknown"} {
		if targets := adobeImageFallbacks(modelID, false); len(targets) != 0 {
			t.Fatalf("native/unknown model %q unexpectedly mapped to %#v", modelID, targets)
		}
	}
}

func TestAdobeImageFallbackWithReferencesNeverUsesGrok(t *testing.T) {
	targets := adobeImageFallbacks("firefly-nano-banana-2", true)
	want := []adobeImageFallbackTarget{
		{provider: "runway", modelID: "nano-banana-2"},
		{provider: "chatgpt", modelID: "gpt-image-2"},
	}
	if !reflect.DeepEqual(targets, want) {
		t.Fatalf("reference-capable fallback chain mismatch: got=%#v want=%#v", targets, want)
	}
}

func TestAdobeImageTemporaryRetryPolicy(t *testing.T) {
	if adobeImageRetryTemporary("firefly-nano-banana-2", false) {
		t.Fatal("third-party model with fallback retained the five-minute temporary retry")
	}
	if !adobeImageRetryTemporary("firefly-image-5", false) {
		t.Fatal("native Firefly model unexpectedly disabled its temporary retry")
	}
}

func TestRunAdobeImageFallbackContinuesToAvailableProvider(t *testing.T) {
	var calls []string
	data, imageURL, err := runAdobeImageFallback(
		&model.ModelConfig{ID: "firefly-nano-banana-2", Provider: "adobe"},
		V1ImageRequest{ReferenceImages: []string{"data:image/png;base64,AA=="}},
		func(*model.ModelConfig, V1ImageRequest) ([]byte, string, error) {
			return nil, "", adobe.ErrTemporaryUpstream
		},
		func(gotItem *model.ModelConfig, _ V1ImageRequest) ([]byte, string, error) {
			calls = append(calls, gotItem.Provider)
			if gotItem.Provider == "runway" {
				return nil, "", ErrNoProviderAccount
			}
			return []byte("chatgpt-image"), "https://example.test/chatgpt.png", nil
		},
	)
	if err != nil || string(data) != "chatgpt-image" || imageURL == "" {
		t.Fatalf("available second fallback did not succeed: data=%q url=%q err=%v", data, imageURL, err)
	}
	if !reflect.DeepEqual(calls, []string{"runway", "chatgpt"}) {
		t.Fatalf("unexpected fallback order: %v", calls)
	}
}
