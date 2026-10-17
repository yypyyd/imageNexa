package service

import (
	"context"
	"time"

	"backend/internal/model"
	"backend/internal/repo"
)

type quotaVerificationKey struct{}

// runQuotaRefresh is shared by manual refresh, maintenance and generation.
// A suppressed request never returns an old balance as a fresh billing proof.
func runQuotaRefresh(ctx context.Context, quotas *repo.QuotaRepository, pool, id string, reason repo.QuotaProbeReason, claim *model.AccountQuotaBucket, fetch func(context.Context) (map[string]any, error)) (map[string]any, error) {
	key, unit, scoped := providerSnapshotScope(pool)
	if quotas == nil || !scoped {
		return fetch(ctx)
	}
	var err error
	if claim == nil {
		claim, err = quotas.ClaimProbe(ctx, id, key, unit, reason, time.Now())
		if err != nil {
			return nil, err
		}
	}
	if claim == nil {
		return map[string]any{"unchanged": true, "unknown": true, "queued": true}, nil
	}
	snapshot, probeErr := fetch(ctx)
	remaining, total, reset, trusted := trustedQuotaSnapshot(snapshot)
	if probeErr != nil || !trusted || boolValueWithDefault(snapshot["unchanged"], false) {
		remaining, total, reset = nil, nil, nil
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	applied, finishErr := quotas.FinishProbe(finishCtx, *claim, total, remaining, reset, time.Now())
	if finishErr != nil {
		return nil, finishErr
	}
	if probeErr != nil {
		return nil, probeErr
	}
	if !applied {
		return unknownQuotaSnapshot(snapshot), nil
	}
	return snapshot, nil
}

func (s *TokenService) refreshClaimedQuota(ctx context.Context, pool string, claim model.AccountQuotaBucket) {
	quotas, _ := s.quotas.(*repo.QuotaRepository)
	_, _ = runQuotaRefresh(ctx, quotas, pool, claim.AccountID, repo.QuotaProbeUsage, &claim, func(ctx context.Context) (map[string]any, error) {
		return s.probeQuota(ctx, pool, claim.AccountID)
	})
	// Provider errors are expected and recorded in the durable backoff. Do not
	// log upstream messages that may contain credentials or proxy URLs.
}

// Import validation already fetches balances for most providers. Publish that
// fresh result instead of immediately sending another identical quota request.
func (s *TokenService) storeImportQuota(ctx context.Context, account model.TokenAccount) (bool, error) {
	remaining, known := jsonMapFloat(account.Meta, "cached_quota_remaining")
	observed, _ := jsonMapInt(account.Meta, "cached_quota_at")
	if !known || remaining < 0 || observed <= 0 || time.Since(time.Unix(int64(observed), 0)) > 2*time.Minute ||
		boolValueWithDefault(account.Meta["quota_probe_unknown"], false) {
		return false, nil
	}
	snapshot := map[string]any{"remaining": remaining, "total": account.Meta["cached_quota_total"], "reset_after": account.CachedQuotaResetAfter}
	return true, s.storeQuotaSnapshot(ctx, account.Pool, account.ID, snapshot)
}

func shouldRefreshDispatchQuota(plan dispatchPlan, pool string, reservation *model.QuotaReservation, generationErr error) bool {
	if pool == "dola" {
		return false
	}
	if bytePlusBetaRetryVerified(generationErr) {
		return true
	}
	// A metered reservation already records successful/accepted consumption.
	return reservation == nil || !reservation.BalanceKnown || !plan.QuotaTracked || plan.Cost <= 0
}
