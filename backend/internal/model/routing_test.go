package model

import (
	"reflect"
	"sort"
	"testing"
)

func TestCanonicalRoutingCatalogIsClosed(t *testing.T) {
	want := []string{
		"firefly-video", "gpt-5-5-mini", "gpt-5-5-thinking", "gpt-image-2",
		"grok-4.5", "grok-chat-fast", "grok-imagine-image", "grok-imagine-video",
		"kling-3", "kling-o3", "luma-ray", "nano-banana-2", "nano-banana-pro",
		"runway-gen-4-turbo", "runway-gen-4.5", "seedance-1.5-pro", "seedance-2.0",
		"seedance-2.0-fast", "seedance-2.0-mini", "seedance-2.5",
		"seedream-5.0-lite", "seedream-5.0-pro", "veo-3.1", "veo-3.1-lite",
	}

	definitions := CanonicalRoutingCatalog()
	got := make([]string, 0, len(definitions))
	seen := make(map[string]bool, len(definitions))
	kinds := map[string]int{}
	for _, definition := range definitions {
		if seen[definition.Model.ID] {
			t.Fatalf("duplicate canonical model %q", definition.Model.ID)
		}
		seen[definition.Model.ID] = true
		got = append(got, definition.Model.ID)
		kinds[definition.Model.Kind]++
		if len(definition.Routes) == 0 {
			t.Fatalf("canonical model %q has no route", definition.Model.ID)
		}
		for _, route := range definition.Routes {
			if route.LogicalModelID != definition.Model.ID {
				t.Fatalf("route %q points to %q, want %q", route.ID, route.LogicalModelID, definition.Model.ID)
			}
			if len(DecodeCapabilityProfiles(route.Capabilities)) == 0 {
				t.Fatalf("route %q has no complete capability profile", route.ID)
			}
			if _, ok := DecodeQuotaCostPolicy(route.QuotaCosts); !ok {
				t.Fatalf("route %q has no explicit quota cost policy", route.ID)
			}
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical IDs changed\n got: %v\nwant: %v", got, want)
	}
	if kinds["text"] != 4 || kinds["image"] != 6 || kinds["video"] != 14 {
		t.Fatalf("canonical kind counts = %#v, want text=4 image=6 video=14", kinds)
	}
	for _, retired := range []string{"lumina-gpt-image-2", "firefly-gpt-image-2", "leonardo-image", "krea-image", "imagine-image"} {
		if IsCanonicalModelID(retired) {
			t.Fatalf("retired provider-specific model %q is public", retired)
		}
	}
}

func TestAdobeRoutesShareOneAllowanceBucket(t *testing.T) {
	for _, definition := range CanonicalRoutingCatalog() {
		for _, route := range definition.Routes {
			if route.Provider == "adobe" && route.QuotaBucketKey != "adobe.credits" {
				t.Fatalf("Adobe route %q bucket = %q, want adobe.credits", route.ID, route.QuotaBucketKey)
			}
		}
	}
}

func TestCanonicalRouteRejectsLegacyProviderUnderCanonicalModel(t *testing.T) {
	legacy := ModelRoute{ID: "image.gpt-image-2.leonardo", LogicalModelID: "gpt-image-2", Provider: "leonardo"}
	if IsCanonicalRoute(legacy) {
		t.Fatal("legacy provider route under a canonical model was accepted")
	}
	custom := ModelRoute{ID: "custom.gpt-image-2", LogicalModelID: "gpt-image-2", Provider: "custom"}
	if !IsCanonicalRoute(custom) {
		t.Fatal("well-formed custom route was rejected")
	}
	canonicalIDWithCorruptShape := ModelRoute{
		ID: "image.gpt-image-2.chatgpt", LogicalModelID: "gpt-image-2", Provider: "byteplus",
	}
	if IsCanonicalRoute(canonicalIDWithCorruptShape) {
		t.Fatal("canonical route id with a corrupt provider identity was accepted")
	}
}

func TestCapabilityMatchingUsesOneCompleteProfile(t *testing.T) {
	route := ModelRoute{Capabilities: capabilityJSON(
		CapabilityProfile{Operations: []string{"generation"}, Ratios: []string{"16:9"}, Resolutions: []string{"1K"}},
		CapabilityProfile{Operations: []string{"generation"}, Ratios: []string{"1:1"}, Resolutions: []string{"4K"}},
	)}

	if !route.Supports(RouteRequirements{Operation: "generation", Ratio: "16:9", Resolution: "1K"}) {
		t.Fatal("route rejected a real capability profile")
	}
	if route.Supports(RouteRequirements{Operation: "generation", Ratio: "16:9", Resolution: "4K"}) {
		t.Fatal("route invented a capability by unioning independent profiles")
	}
}
