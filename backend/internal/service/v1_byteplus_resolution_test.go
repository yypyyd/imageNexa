package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"backend/internal/model"
	"backend/internal/provider/byteplus"
	"gorm.io/datatypes"
)

func TestLuminaGPTImage2QualityIsIndependentFromResolution(t *testing.T) {
	item := &model.ModelConfig{
		ID:     "lumina-gpt-image-2",
		Prices: datatypes.JSONMap{"1K": 1, "2K": 2, "4K": 4},
	}

	ratio, resolution := resolveImageSize(item, V1ImageRequest{
		Size:    "2480x3312",
		Quality: "low",
	})
	if ratio != "3:4" || resolution != "2K" {
		t.Fatalf("resolveImageSize() = %q, %q; want 3:4, 2K", ratio, resolution)
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

func TestBytePlusRetryableBetaFailureAllowsOnlyAccountFailover(t *testing.T) {
	err := errors.Join(byteplus.ErrTaskAccepted, byteplus.ErrRetryableTaskFailed, errBytePlusRetryVerified)
	if !byteplusNoResubmit(err) {
		t.Fatal("provider-level accepted marker must remain conservative")
	}
	if !noRouteFailover(err) {
		t.Fatal("beta account retry must not authorize another provider route")
	}
	isAuth, isQuota, isTemporary, isDead := byteplusErrClass(err)
	if isAuth || isQuota || !isTemporary || isDead {
		t.Fatalf("byteplusErrClass(beta) = (%v, %v, %v, %v), want temporary account failover", isAuth, isQuota, isTemporary, isDead)
	}
	state, class := dispatchFailureClass(err)
	if state != "failed" || class != "temporary" {
		t.Fatalf("dispatchFailureClass(beta) = %q, %q; want failed, temporary", state, class)
	}
	if got := mapBytePlusImageError(err); !errors.Is(got, ErrProviderTemporary) {
		t.Fatalf("mapBytePlusImageError(beta) = %v, want temporary", got)
	}
}

func TestBytePlusBetaChainDeadlineKeepsRouteFailoverBlocked(t *testing.T) {
	verified := verifiedBytePlusRetry(errors.Join(byteplus.ErrTaskAccepted, byteplus.ErrRetryableTaskFailed), 68)
	terminal := errors.Join(verified, byteplus.ErrQuotaExhausted, context.DeadlineExceeded)
	if !noRouteFailover(terminal) {
		t.Fatal("deadline during the beta account chain must retain the no-route-failover marker")
	}
	if got := publicGenerationError(terminal); !errors.Is(got, ErrProviderQuota) {
		t.Fatalf("publicGenerationError() = %v, want the alternate's concrete quota class", got)
	}
}

func TestBytePlusUnverifiedBetaFailureCannotFailOver(t *testing.T) {
	err := errors.Join(byteplus.ErrTaskAccepted, byteplus.ErrRetryableTaskFailed)
	isAuth, isQuota, isTemporary, isDead := byteplusErrClass(err)
	if isAuth || isQuota || isTemporary || isDead {
		t.Fatalf("byteplusErrClass(unverified beta) = (%v, %v, %v, %v), want all false", isAuth, isQuota, isTemporary, isDead)
	}
	state, class := dispatchFailureClass(err)
	if state != "failed" || class != "temporary" {
		t.Fatalf("dispatchFailureClass(unverified beta) = %q, %q; want failed, temporary", state, class)
	}
}

func TestFinalBytePlusBalanceCanRevokeRetryPermission(t *testing.T) {
	raw := errors.Join(byteplus.ErrTaskAccepted, byteplus.ErrRetryableTaskFailed)
	verified := verifiedBytePlusRetry(raw, 68)
	unchanged := 68.0
	if got := finalizeBytePlusRetryVerification(verified, &unchanged); !bytePlusBetaRetryVerified(got) {
		t.Fatalf("unchanged final balance revoked retry: %v", got)
	}
	decreased := 43.0
	for _, remaining := range []*float64{&decreased, nil} {
		got := finalizeBytePlusRetryVerification(verified, remaining)
		if bytePlusBetaRetryVerified(got) {
			t.Fatalf("final balance %v retained retry permission: %v", remaining, got)
		}
		if !errors.Is(got, byteplus.ErrTaskAccepted) || !errors.Is(got, byteplus.ErrRetryableTaskFailed) {
			t.Fatalf("downgraded error lost provider markers: %v", got)
		}
	}
}

func TestBytePlusBetaRetryBudgetAllowsSixDistinctAccountAttempts(t *testing.T) {
	now := time.Date(2026, 9, 5, 2, 16, 49, 0, time.UTC)
	var budget bytePlusBetaRetryBudget
	for attempt := 1; attempt <= maxBytePlusBetaAccountAttempts; attempt++ {
		failure := verifiedBytePlusRetry(errors.Join(byteplus.ErrTaskAccepted, byteplus.ErrRetryableTaskFailed), 68)
		retry, terminal := budget.handle(failure, now)
		if attempt < maxBytePlusBetaAccountAttempts {
			if !retry || terminal != nil {
				t.Fatalf("attempt %d = (retry %v, terminal %v), want another account", attempt, retry, terminal)
			}
		} else {
			if retry || !errors.Is(terminal, errBytePlusRetryBudgetExhausted) || !errors.Is(terminal, byteplus.ErrRetryableTaskFailed) {
				t.Fatalf("final attempt = (retry %v, terminal %v), want exhausted retryable beta error", retry, terminal)
			}
		}
		wantDeadline := now.Add(providerAccountQueueWait)
		if !budget.nextAccountDeadline.Equal(wantDeadline) {
			t.Fatalf("attempt %d deadline = %v, want %v", attempt, budget.nextAccountDeadline, wantDeadline)
		}
		now = now.Add(2 * time.Minute)
	}
	if budget.attempts != maxBytePlusBetaAccountAttempts {
		t.Fatalf("attempt count = %d, want %d", budget.attempts, maxBytePlusBetaAccountAttempts)
	}
}

func TestBytePlusBetaRetryBudgetStopsOnUnsafeAlternateOutcome(t *testing.T) {
	start := verifiedBytePlusRetry(errors.Join(byteplus.ErrTaskAccepted, byteplus.ErrRetryableTaskFailed), 68)
	tests := []struct {
		name             string
		err              error
		wantBudgetMarker bool
	}{
		{
			name: "accepted outcome keeps native no-resubmit marker",
			err:  byteplus.ErrTaskSubmissionUnknown,
		},
		{
			name:             "definite alternate error cannot cross routes",
			err:              byteplus.ErrQuotaExhausted,
			wantBudgetMarker: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var budget bytePlusBetaRetryBudget
			if retry, terminal := budget.handle(start, time.Now()); !retry || terminal != nil {
				t.Fatalf("start = (retry %v, terminal %v), want retry", retry, terminal)
			}
			retry, terminal := budget.handle(test.err, time.Now())
			if retry || !errors.Is(terminal, test.err) {
				t.Fatalf("alternate = (retry %v, terminal %v), want original terminal", retry, terminal)
			}
			if errors.Is(terminal, errBytePlusRetryBudgetExhausted) != test.wantBudgetMarker {
				t.Fatalf("budget marker = %v, want %v (error %v)", errors.Is(terminal, errBytePlusRetryBudgetExhausted), test.wantBudgetMarker, terminal)
			}
		})
	}
}

func TestBytePlusGenericAcceptedTaskFailureStillCannotFailOver(t *testing.T) {
	err := errors.Join(byteplus.ErrTaskAccepted, byteplus.ErrTaskFailed)
	isAuth, isQuota, isTemporary, isDead := byteplusErrClass(err)
	if isAuth || isQuota || isTemporary || isDead {
		t.Fatalf("byteplusErrClass(generic accepted failure) = (%v, %v, %v, %v), want all false", isAuth, isQuota, isTemporary, isDead)
	}
	state, class := dispatchFailureClass(err)
	if state != "accepted" || class != "temporary" {
		t.Fatalf("dispatchFailureClass(generic accepted failure) = %q, %q; want accepted, temporary", state, class)
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
