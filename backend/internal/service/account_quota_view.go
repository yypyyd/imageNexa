package service

import (
	"context"
	"time"

	"backend/internal/model"
)

func primaryQuotaBucket(provider string, now time.Time) string {
	if provider == "dola" {
		return model.DolaVideoBucketAt(now)
	}
	key, _, _ := providerSnapshotScope(provider)
	return key
}

// Dola creates its daily ledger on first use. A missing ledger means the full
// daily allowance, not an unknown quota. Existing balances remain authoritative.
func accountQuotaViewBuckets(provider string, buckets []model.AccountQuotaBucket, now time.Time) []model.AccountQuotaBucket {
	if provider != "dola" {
		return buckets
	}
	key := model.DolaVideoBucketAt(now)
	for _, bucket := range buckets {
		if bucket.BucketKey == key {
			return buckets
		}
	}
	total, remaining := float64(model.DolaDailyVideoLimit), float64(model.DolaDailyVideoLimit)
	day, _ := model.DolaVideoBucketDay(key)
	reset := day.AddDate(0, 0, 1)
	view := append([]model.AccountQuotaBucket(nil), buckets...)
	return append(view, model.AccountQuotaBucket{
		BucketKey: key, Unit: "generations", Total: &total, Remaining: &remaining, ResetAt: &reset,
	})
}

func accountQuotaState(provider string, buckets []model.AccountQuotaBucket, now time.Time) string {

	key := primaryQuotaBucket(provider, now)
	for _, bucket := range accountQuotaViewBuckets(provider, buckets, now) {
		if bucket.BucketKey != key {
			continue
		}
		if bucket.Remaining == nil {
			return "unknown"
		}
		if *bucket.Remaining <= 0 {
			return "empty"
		}
		return "available"
	}
	return "unknown"
}

// Aggregate the same primary bucket shown on the account row. Obsolete Grok
// text buckets and previous Dola days cannot inflate today's media capacity.
func (s *AdminConsoleService) countQuotaHealth(ctx context.Context, counts map[string]map[string]int64) error {
	now := time.Now()
	for _, provider := range adminAccountProviders {
		key := primaryQuotaBucket(provider, now)
		if key == "" {
			continue
		}
		type quotaCount struct {
			State string
			Count int64
		}
		var rows []quotaCount
		if err := s.db.WithContext(ctx).Raw(`SELECT CASE
			WHEN a.pool = 'dola' AND q.id IS NULL THEN 'quota_available'
			WHEN q.id IS NULL OR q.remaining IS NULL THEN 'quota_unknown'
			WHEN q.remaining <= 0 THEN 'quota_empty'
			ELSE 'quota_available' END AS state, COUNT(*) AS count
			FROM provider_accounts a LEFT JOIN account_quota_buckets q ON q.account_id=a.id AND q.bucket_key=?
			WHERE a.pool=? AND a.status='active' AND a.dead=false GROUP BY 1`, key, provider).Scan(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			counts[provider][row.State] = row.Count
		}
	}
	return nil
}
