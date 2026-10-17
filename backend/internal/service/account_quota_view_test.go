package service

import (
	"testing"
	"time"

	"backend/internal/model"
)

func TestDolaQuotaViewDailyAllowance(t *testing.T) {
	now := time.Date(2026, 10, 17, 1, 0, 0, 0, time.UTC)
	key := model.DolaVideoBucketAt(now)
	zero, one := 0.0, 1.0
	for _, tc := range []struct {
		name      string
		buckets   []model.AccountQuotaBucket
		remaining float64
		state     string
	}{
		{"unused", nil, 2, "available"},
		{"new day", []model.AccountQuotaBucket{{BucketKey: model.DolaVideoBucketAt(now.Add(-24 * time.Hour)), Remaining: &zero}}, 2, "available"},
		{"used once", []model.AccountQuotaBucket{{BucketKey: key, Remaining: &one}}, 1, "available"},
		{"exhausted", []model.AccountQuotaBucket{{BucketKey: key, Remaining: &zero}}, 0, "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := accountQuotaViewBuckets("dola", tc.buckets, now)
			var current *model.AccountQuotaBucket
			for i := range view {
				if view[i].BucketKey == key {
					current = &view[i]
				}
			}
			if current == nil || current.Remaining == nil || *current.Remaining != tc.remaining {
				t.Fatalf("unexpected daily quota: %#v", current)
			}
			if got := accountQuotaState("dola", tc.buckets, now); got != tc.state {
				t.Fatalf("state=%s want=%s", got, tc.state)
			}
			if tc.remaining == 2 && (current.Total == nil || *current.Total != 2 || current.ResetAt == nil || !current.ResetAt.Equal(time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC))) {
				t.Fatalf("invalid daily allowance/reset: %#v", current)
			}
		})
	}
}

func TestDolaAccountRowIncludesUnusedAllowance(t *testing.T) {
	row := assembleAccountRow(model.TokenAccount{Pool: "dola", Status: "active"}, nil, nil, nil, 0)
	if row["quota_state"] != "available" {
		t.Fatalf("unexpected state: %v", row["quota_state"])
	}
	buckets := row["quota_buckets"].([]map[string]any)
	if len(buckets) != 1 || buckets[0]["name"] != row["primary_quota_bucket"] || *buckets[0]["remaining"].(*float64) != 2 {
		t.Fatalf("missing daily allowance: %#v", buckets)
	}
}
