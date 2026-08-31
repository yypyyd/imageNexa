package service

import (
	"errors"
	"fmt"
	"testing"

	"backend/internal/model"
	"backend/internal/provider/byteplus"
	"gorm.io/datatypes"
)

func TestLuminaGPTImage2QualitySelectsResolution(t *testing.T) {
	item := &model.ModelConfig{
		ID:     "lumina-gpt-image-2",
		Prices: datatypes.JSONMap{"1K": 1, "2K": 2, "4K": 4},
	}

	ratio, resolution := resolveImageSize(item, V1ImageRequest{
		Size:    "2480x3312",
		Quality: "high",
	})
	if ratio != "3:4" || resolution != "4K" {
		t.Fatalf("resolveImageSize() = %q, %q; want 3:4, 4K", ratio, resolution)
	}

	_, explicit := resolveImageSize(item, V1ImageRequest{
		Size:       "2480x3312",
		Quality:    "high",
		Resolution: "2K",
	})
	if explicit != "2K" {
		t.Fatalf("explicit resolution = %q, want 2K", explicit)
	}
}

func TestLuminaGPTImage2QualityModelMatchIsNormalized(t *testing.T) {
	if !supportsQualityResolutionModel("  LUMINA-GPT-IMAGE-2  ") {
		t.Fatal("lumina-gpt-image-2 must participate in quality-to-resolution mapping")
	}
}

func TestBytePlusNoResubmitErrorsAreTemporaryButNeverFailOver(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("generation failed: %w", byteplus.ErrTaskAccepted),
		fmt.Errorf("generation failed: %w", byteplus.ErrTaskSubmissionUnknown),
		errors.Join(byteplus.ErrTaskAccepted, byteplus.ErrQuotaExhausted),
		errors.Join(byteplus.ErrTaskAccepted, byteplus.ErrRiskControl),
		errors.Join(byteplus.ErrTaskAccepted, byteplus.ErrInvalidParams),
	} {
		if !errors.Is(err, byteplus.ErrTemporaryUpstream) {
			t.Fatalf("%v must remain a temporary upstream error for API mapping", err)
		}
		isAuth, isQuota, isTemporary, isDead := byteplusErrClass(err)
		if isAuth || isQuota || isTemporary || isDead {
			t.Fatalf("byteplusErrClass(%v) = (%v, %v, %v, %v), want all false", err, isAuth, isQuota, isTemporary, isDead)
		}
	}
}

func TestMapBytePlusImageErrorPrioritizesAcceptedBusinessOutcome(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{name: "accepted quota", err: errors.Join(byteplus.ErrTaskAccepted, byteplus.ErrQuotaExhausted), want: ErrProviderQuota},
		{name: "accepted risk", err: errors.Join(byteplus.ErrTaskAccepted, byteplus.ErrRiskControl), want: ErrContentRejected},
		{name: "accepted invalid", err: errors.Join(byteplus.ErrTaskAccepted, byteplus.ErrInvalidParams), want: ErrUnsupportedParams},
		{name: "accepted temporary", err: byteplus.ErrTaskAccepted, want: ErrProviderTemporary},
		{name: "ambiguous submit", err: byteplus.ErrTaskSubmissionUnknown, want: ErrProviderTemporary},
		{name: "accepted auth-like", err: errors.Join(byteplus.ErrTaskAccepted, byteplus.ErrAuth), want: ErrProviderTemporary},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mapBytePlusImageError(tc.err)
			if !errors.Is(got, tc.want) {
				t.Fatalf("mapBytePlusImageError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestFilterBytePlusAccountsByExactCredits(t *testing.T) {
	items := []model.TokenAccount{
		{ID: "fractional", Meta: datatypes.JSONMap{"cached_quota_remaining": 3.5}},
		{ID: "too-small", Meta: datatypes.JSONMap{"cached_quota_remaining": 3.4}},
		{ID: "unknown"},
	}
	eligible, insufficient := filterBytePlusAccountsByCredits(items, 3.5)
	if !insufficient {
		t.Fatal("known insufficient balance was not reported")
	}
	if len(eligible) != 2 || eligible[0].ID != "fractional" || eligible[1].ID != "unknown" {
		t.Fatalf("eligible = %#v, want fractional and unknown", eligible)
	}
}

func TestPrioritizeBytePlusAccountsUsesBestFitWithinWeight(t *testing.T) {
	items := []model.TokenAccount{
		{ID: "unknown", Weight: 1},
		{ID: "large", Weight: 1, Meta: datatypes.JSONMap{"cached_quota_remaining": 230}},
		{ID: "small", Weight: 1, Meta: datatypes.JSONMap{"cached_quota_remaining": 12}},
		{ID: "priority", Weight: 2, Meta: datatypes.JSONMap{"cached_quota_remaining": 500}},
	}
	service := &V1Service{}
	service.prioritizeBytePlusAccounts(items)
	want := []string{"priority", "small", "large", "unknown"}
	for index, id := range want {
		if items[index].ID != id {
			t.Fatalf("ordered[%d] = %q, want %q (all=%#v)", index, items[index].ID, id, items)
		}
	}
}
