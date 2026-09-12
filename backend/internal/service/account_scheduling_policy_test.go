package service

import (
	"errors"
	"testing"

	"backend/internal/model"
	"backend/internal/provider/adobe"
	"backend/internal/provider/chatgpt"
)

func TestTempFailureSignatureStable(t *testing.T) {
	if tempFailureSignature(adobe.ErrTemporaryUpstream) != "adobe.temporary" {
		t.Fatalf("adobe signature")
	}
	wrapped := errors.Join(adobe.ErrTemporaryUpstream, errors.New("provider temporarily unavailable"))
	if tempFailureSignature(wrapped) != "adobe.temporary" {
		t.Fatalf("wrapped adobe signature")
	}
	if tempFailureSignature(chatgpt.ErrTemporaryUpstream) == tempFailureSignature(adobe.ErrTemporaryUpstream) {
		t.Fatalf("provider signatures must differ")
	}
}

func TestTempFailoverExcludesAndStopsOnCorrelatedErrors(t *testing.T) {
	var state tempFailoverState
	if !excludeAfterAccountFailure(false, false, false, true, adobe.ErrTemporaryUpstream) {
		t.Fatalf("temp failure must exclude the account")
	}
	if state.note(adobe.ErrTemporaryUpstream) {
		t.Fatalf("first unique temp failure should continue")
	}
	if state.note(adobe.ErrTemporaryUpstream) {
		t.Fatalf("second identical temp failure should continue")
	}
	if !state.note(adobe.ErrTemporaryUpstream) {
		t.Fatalf("three identical temp failures are a pool outage")
	}
	if state.unique != 3 || state.sameSig != 3 {
		t.Fatalf("got unique=%d same=%d", state.unique, state.sameSig)
	}
}

func TestTempFailoverAllowsSecondWaveOfDifferentErrors(t *testing.T) {
	var state tempFailoverState
	for i := 0; i < maxTempFailoverAccounts-1; i++ {
		err := chatgpt.ErrTemporaryUpstream
		if i%2 == 0 {
			err = adobe.ErrTemporaryUpstream
		}
		if state.note(err) {
			t.Fatalf("mixed temp failures should explore unused accounts, stop at i=%d", i)
		}
	}
	if !state.note(adobe.ErrTemporaryUpstream) {
		t.Fatalf("unique-account budget must still cap fan-out")
	}
	if state.unique != maxTempFailoverAccounts {
		t.Fatalf("unique=%d", state.unique)
	}
}

func TestSortDemoteCoolingPreservesHealthyOrder(t *testing.T) {
	items := []model.TokenAccount{
		{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"},
	}
	sortDemoteCooling(items, map[string]bool{"b": true, "d": true})
	got := []string{items[0].ID, items[1].ID, items[2].ID, items[3].ID}
	want := []string{"a", "c", "b", "d"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestRouteDispatchModelIDPrefersAdobeRuntime(t *testing.T) {
	route := model.ModelRoute{Provider: "adobe", LogicalModelID: "kling-3", RuntimeModel: "firefly-kling-3"}
	if got := routeDispatchModelID(route); got != "firefly-kling-3" {
		t.Fatalf("adobe runtime: %s", got)
	}
	custom := model.ModelRoute{Provider: "custom", LogicalModelID: "my-model", RuntimeModel: "upstream-name"}
	if got := routeDispatchModelID(custom); got != "my-model" {
		t.Fatalf("custom logical: %s", got)
	}
}

func TestAdobeVideoEligibilityAgreesOnLogicalAndRuntimeIDs(t *testing.T) {
	ordinary := model.TokenAccount{Meta: map[string]any{"cached_quota_total": 10}}
	if !adobeAccountSupportsModel(ordinary, "firefly-video", "video") {
		t.Fatalf("ordinary firefly-video")
	}
	if adobeAccountSupportsModel(ordinary, "kling-3", "video") || adobeAccountSupportsModel(ordinary, "firefly-kling-3", "video") {
		t.Fatalf("kling is points-only on both public and runtime ids")
	}
}

func TestExcludeAfterAccountFailureClasses(t *testing.T) {
	if !excludeAfterAccountFailure(true, false, false, false, adobe.ErrAuth) {
		t.Fatalf("auth")
	}
	if !excludeAfterAccountFailure(false, true, false, false, adobe.ErrQuotaExhausted) {
		t.Fatalf("quota")
	}
	if !excludeAfterAccountFailure(false, false, false, false, ErrNoProviderAccount) {
		t.Fatalf("no account")
	}
	if excludeAfterAccountFailure(false, false, false, false, errors.New("bad prompt")) {
		t.Fatalf("request-level errors stay on the caller")
	}
}
