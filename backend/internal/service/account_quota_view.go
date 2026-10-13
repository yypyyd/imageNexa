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

func accountQuotaState(provider string, buckets []model.AccountQuotaBucket, now time.Time) string {
	if provider == "custom" {
		return "unmetered"
	}
	key := primaryQuotaBucket(provider, now)
	for _, bucket := range buckets {
		if bucket.BucketKey != key {
			continue
		}
		if bucket.Remaining == nil {
			return "unknown"
		}
		if *bucket.Remaining <= 0 {
			return "empty"
		}
		if provider != "dola" && (bucket.RefreshedAt == nil || now.Sub(*bucket.RefreshedAt) > 30*time.Minute) {
			return "stale"
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
			WHEN q.id IS NULL OR q.remaining IS NULL THEN 'quota_unknown'
			WHEN q.remaining <= 0 THEN 'quota_empty'
			WHEN a.pool <> 'dola' AND (q.refreshed_at IS NULL OR q.refreshed_at < ?) THEN 'quota_stale'
			ELSE 'quota_available' END AS state, COUNT(*) AS count
			FROM provider_accounts a LEFT JOIN account_quota_buckets q ON q.account_id=a.id AND q.bucket_key=?
			WHERE a.pool=? AND a.status='active' AND a.dead=false GROUP BY 1`, now.Add(-30*time.Minute), key, provider).Scan(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			counts[provider][row.State] = row.Count
		}
	}
	return nil
}
