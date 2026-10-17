package repo

import (
	"context"
	"time"

	"backend/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const QuotaRefreshInterval = 24 * time.Hour
const quotaProbeLease = 5 * time.Minute

type QuotaProbeReason int

const (
	QuotaProbeManual QuotaProbeReason = iota
	QuotaProbeUsage
	QuotaProbeRecovery
	QuotaProbeVerification // A fresh balance is required to prove safe retry.
)

func quotaRetryDelay(failures int) time.Duration {
	switch failures {
	case 0, 1:
		return 15 * time.Minute
	case 2:
		return time.Hour
	case 3:
		return 6 * time.Hour
	default:
		return QuotaRefreshInterval
	}
}

func nextQuotaRefresh(bucket model.AccountQuotaBucket, now time.Time) time.Time {
	next := now.Add(QuotaRefreshInterval)
	// Runway/Oreate expiration markers are not allowance renewals.
	if bucket.BucketKey != "runway.credits" && bucket.BucketKey != "oreate.points" &&
		bucket.Remaining != nil && *bucket.Remaining <= 0 && bucket.ResetAt != nil &&
		bucket.ResetAt.After(now) && bucket.ResetAt.Before(next) {
		next = *bucket.ResetAt
	}
	return next
}

func claimQuotaProbe(tx *gorm.DB, bucket *model.AccountQuotaBucket, now time.Time) error {
	until := now.Add(quotaProbeLease)
	bucket.ProbeID, bucket.ProbeUntil, bucket.LastProbeAt = durableID("qp_"), &until, &now
	return tx.Model(bucket).Updates(map[string]any{
		"probe_id": bucket.ProbeID, "probe_until": until, "last_probe_at": now,
	}).Error
}

// ClaimProbe serializes manual, generation and maintenance probes across API
// processes. A later generation requests a follow-up instead of using a balance
// captured before that generation completed.
func (r *QuotaRepository) ClaimProbe(ctx context.Context, accountID, key, unit string, reason QuotaProbeReason, now time.Time) (*model.AccountQuotaBucket, error) {
	var claimed *model.AccountQuotaBucket
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		initial := model.AccountQuotaBucket{ID: quotaBucketID(accountID, key), AccountID: accountID, BucketKey: key, Unit: unit}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&initial).Error; err != nil {
			return err
		}
		var bucket model.AccountQuotaBucket
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&bucket, "id = ?", initial.ID).Error; err != nil {
			return err
		}
		if bucket.ProbeUntil != nil && bucket.ProbeUntil.After(now) {
			if reason == QuotaProbeUsage {
				return tx.Model(&bucket).UpdateColumn("refresh_requested_at", now).Error
			}
			return nil
		}
		if (reason == QuotaProbeUsage || reason == QuotaProbeRecovery) && bucket.RefreshFailures > 0 && bucket.RefreshAfter != nil && bucket.RefreshAfter.After(now) {
			return nil
		}
		if reason != QuotaProbeVerification && reason != QuotaProbeRecovery && bucket.Reserved > 0 {
			next := now.Add(time.Minute)
			return tx.Model(&bucket).Updates(map[string]any{"refresh_after": next, "refresh_requested_at": now}).Error
		}
		if reason != QuotaProbeVerification {
			if bucket.LastProbeAt != nil && now.Sub(*bucket.LastProbeAt) < time.Minute {
				if reason == QuotaProbeUsage {
					return tx.Model(&bucket).Updates(map[string]any{"refresh_after": bucket.LastProbeAt.Add(time.Minute), "refresh_requested_at": now}).Error
				}
				return nil
			}
		}
		if err := claimQuotaProbe(tx, &bucket, now); err != nil {
			return err
		}
		bucket.ProbeAllowsReservations = reason == QuotaProbeVerification || reason == QuotaProbeRecovery
		claimed = &bucket
		return nil
	})
	return claimed, err
}

// FinishProbe applies only the current lease's result. Failures never change the
// balance or its observation time, and retry state survives restarts.
func (r *QuotaRepository) FinishProbe(ctx context.Context, claim model.AccountQuotaBucket, total, remaining *float64, reset *time.Time, now time.Time) (bool, error) {
	applied := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var bucket model.AccountQuotaBucket
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&bucket, "id = ?", claim.ID).Error; err != nil {
			return err
		}
		if bucket.ProbeID != claim.ProbeID || claim.ProbeID == "" {
			return nil
		}
		// A snapshot racing a local debit cannot prove whether the provider has
		// already included that debit. Keep the ledger and retry once work settles.
		if remaining != nil && !claim.ProbeAllowsReservations && (bucket.Reserved > 0 || bucket.Revision != claim.Revision) {
			next := now.Add(time.Minute)
			bucket.RefreshAfter, bucket.RefreshRequestedAt = &next, &now
			bucket.ProbeID, bucket.ProbeUntil = "", nil
			if err := tx.Save(&bucket).Error; err != nil {
				return err
			}
			return syncQuotaMetadata(tx, bucket)
		}
		if remaining != nil {
			applyQuotaSnapshot(&bucket, claim.Unit, total, remaining, reset, now)
			applied = true
		} else if bucket.RefreshedAt == nil || claim.LastProbeAt == nil || bucket.RefreshedAt.Before(*claim.LastProbeAt) {
			bucket.RefreshFailures = min(bucket.RefreshFailures+1, 4)
			next := now.Add(quotaRetryDelay(bucket.RefreshFailures))
			bucket.RefreshAfter = &next
		}
		if applied && bucket.RefreshRequestedAt != nil && claim.LastProbeAt != nil && bucket.RefreshRequestedAt.After(*claim.LastProbeAt) {
			next := now.Add(time.Minute)
			bucket.RefreshAfter = &next
		}
		bucket.ProbeID, bucket.ProbeUntil, bucket.RefreshRequestedAt = "", nil, nil
		if err := tx.Save(&bucket).Error; err != nil {
			return err
		}
		if err := syncQuotaMetadata(tx, bucket); err != nil {
			return err
		}
		if applied {
			return restoreQuotaAccount(tx, bucket)
		}
		return nil
	})
	return applied, err
}

func restoreQuotaAccount(tx *gorm.DB, bucket model.AccountQuotaBucket) error {
	if bucket.Remaining == nil || *bucket.Remaining <= 0 {
		return nil
	}
	pool := map[string]string{"chatgpt.image": "chatgpt", "grok.media": "grok", "byteplus.computing_points": "byteplus"}[bucket.BucketKey]
	if pool == "" {
		return nil
	}
	return tx.Model(&model.TokenAccount{}).Where("id = ? AND pool = ? AND status = 'quota' AND dead = false", bucket.AccountID, pool).
		Updates(map[string]any{"status": "active", "quota_recover_at": nil, "fails": 0}).Error
}

// RequestRefresh brings a recovered account into the regular bounded queue.
// An existing error backoff is retained; recovery cannot start a retry storm.
func (r *QuotaRepository) RequestRefresh(ctx context.Context, accountID, key string) error {
	now := time.Now()
	return r.db.WithContext(ctx).Model(&model.AccountQuotaBucket{}).
		Where("account_id = ? AND bucket_key = ? AND refresh_failures = 0", accountID, key).
		Updates(map[string]any{"refresh_after": now, "refresh_requested_at": now}).Error
}

func applyQuotaSnapshot(bucket *model.AccountQuotaBucket, unit string, total, upstreamRemaining *float64, reset *time.Time, now time.Time) {
	bucket.Unit = unit
	if total != nil {
		bucket.Total = cloneFloat(total)
	}
	if reset != nil {
		bucket.ResetAt = reset
	}
	if upstreamRemaining != nil {
		available := max(0, *upstreamRemaining-bucket.Reserved)
		bucket.Remaining = &available
	}
	bucket.RefreshedAt, bucket.Revision, bucket.UpdatedAt = &now, bucket.Revision+1, now
	bucket.RefreshFailures = 0
	next := nextQuotaRefresh(*bucket, now)
	bucket.RefreshAfter = &next
}

// Adapter filters still read cached_quota_remaining. Keep that mirror in the
// same transaction as the authoritative ledger, including local deductions.
func syncQuotaMetadata(tx *gorm.DB, bucket model.AccountQuotaBucket) error {
	if bucket.Remaining == nil {
		return nil
	}
	pool := map[string]string{"chatgpt.image": "chatgpt", "byteplus.computing_points": "byteplus", "grok.media": "grok", "adobe.credits": "adobe", "runway.credits": "runway", "oreate.points": "oreate"}[bucket.BucketKey]
	if pool == "" {
		return nil
	}
	return tx.Exec(`UPDATE provider_accounts SET meta = (CASE WHEN jsonb_typeof(meta) = 'object' THEN meta ELSE '{}'::jsonb END) || jsonb_build_object(
		'cached_quota_remaining', ?::numeric, 'cached_quota_reserved', ?::numeric), updated_at = NOW()
		WHERE id = ? AND pool = ?`, *bucket.Remaining, bucket.Reserved, bucket.AccountID, pool).Error
}
