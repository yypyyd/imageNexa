package service

import (
	"context"
	"testing"
	"time"

	"backend/internal/model"
	"backend/internal/repo"
	"github.com/DATA-DOG/go-sqlmock"
)

type quotaSnapshotSpy struct {
	calls                 int
	account, bucket, unit string
	remaining             float64
}

func (s *quotaSnapshotSpy) UpsertSnapshot(ctx context.Context, account, bucket, unit string, total, remaining *float64, reset *time.Time) (*model.AccountQuotaBucket, error) {
	s.calls++
	s.account, s.bucket, s.unit, s.remaining = account, bucket, unit, *remaining
	return &model.AccountQuotaBucket{}, nil
}

func TestQuotaProbePublishesRecoveredBalanceToSchedulingBucket(t *testing.T) {
	db, mock := schedulingDB(t)
	account := schedulingAccount("recovered")
	account.Pool = "byteplus"
	mock.ExpectQuery(`SELECT .* FROM "provider_accounts"`).WillReturnRows(schedulingRows(account))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT .* FROM "provider_accounts".*FOR UPDATE`).WillReturnRows(schedulingRows(account))
	mock.ExpectExec(`UPDATE "provider_accounts"`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	store := &quotaSnapshotSpy{}
	s := &TokenService{tokens: repo.NewTokenRepository(db), byteplus: &reorderedBytePlusProbeClient{}, quotas: store}
	data, err := s.Quota(context.Background(), "byteplus", account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if data["remaining"] != 7 || store.calls != 1 || store.account != account.ID || store.bucket != "byteplus.computing_points" || store.unit != "points" || store.remaining != 7 {
		t.Fatalf("recovery did not publish quota: data=%v store=%+v", data, store)
	}
}

func TestUntrustedQuotaCannotOverwriteSchedulingBalance(t *testing.T) {
	for _, snapshot := range []map[string]any{
		{"remaining": 0, "unknown": true},
		{"remaining": 7, "unchanged": true},
		{"remaining": 0, "auth_failed": true},
		{"total": 10},
		{"remaining": -1},
	} {
		store := &quotaSnapshotSpy{}
		s := &TokenService{quotas: store}
		if err := s.storeQuotaSnapshot(context.Background(), "chatgpt", "a", snapshot); err != nil {
			t.Fatal(err)
		}
		if store.calls != 0 {
			t.Fatalf("untrusted probe was published: %v", snapshot)
		}
	}
}

func TestRecoverySnapshotPreservesConcurrentReservations(t *testing.T) {
	db, mock := schedulingDB(t)
	old := time.Now().Add(-time.Hour)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT .* FROM "account_quota_buckets".*FOR UPDATE`).WillReturnRows(
		sqlmock.NewRows([]string{"id", "account_id", "bucket_key", "unit", "remaining", "reserved", "refreshed_at", "revision"}).
			AddRow("a:chatgpt.image", "a", "chatgpt.image", "generations", 0, 2, old, 1))
	mock.ExpectExec(`UPDATE "account_quota_buckets"`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	remaining := 10.0
	bucket, err := repo.NewQuotaRepository(db).UpsertSnapshot(context.Background(), "a", "chatgpt.image", "generations", nil, &remaining, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bucket.Remaining == nil || *bucket.Remaining != 8 || bucket.Reserved != 2 {
		t.Fatalf("recovery erased holds: %+v", bucket)
	}
}

func TestQuotaRefreshClaimThrottlesProbesWithoutInventingFreshBalance(t *testing.T) {
	db, mock := schedulingDB(t)
	now := time.Now().Truncate(time.Second)
	old := now.Add(-time.Hour)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT .* FROM "account_quota_buckets".*FOR UPDATE SKIP LOCKED`).
		WithArgs(now.Add(-5*time.Minute), now.Add(-15*time.Minute), now, 20).
		WillReturnRows(sqlmock.NewRows([]string{"id", "account_id", "bucket_key", "remaining", "refreshed_at", "updated_at"}).
			AddRow("a:chatgpt.image", "a", "chatgpt.image", 0, old, old))
	mock.ExpectExec(`UPDATE "account_quota_buckets" SET "updated_at"`).WithArgs(now, "a:chatgpt.image").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	buckets, err := repo.NewQuotaRepository(db).ClaimRefreshDue(context.Background(), now, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 || !buckets[0].RefreshedAt.Equal(old) || *buckets[0].Remaining != 0 {
		t.Fatalf("claim changed authoritative balance/freshness: %+v", buckets)
	}
}

type unknownBalanceClient struct{}

func (*unknownBalanceClient) FetchProfile(context.Context, string) (map[string]any, error) {
	return nil, nil
}
func (*unknownBalanceClient) FetchCreditsBalance(context.Context, string) (map[string]any, error) {
	return map[string]any{"remaining": 0, "unknown": true, "error": "sensitive provider diagnostic"}, nil
}

func TestUnknownProbePreservesDisplayCacheAndSchedulingBucket(t *testing.T) {
	db, mock := schedulingDB(t)
	account := schedulingAccount("a")
	account.Pool = "byteplus"
	// No UPDATE or transaction is expected: a challenged balance endpoint must
	// not replace either quota source with its placeholder zero.
	mock.ExpectQuery(`SELECT .* FROM "provider_accounts"`).WillReturnRows(schedulingRows(account))
	store := &quotaSnapshotSpy{}
	s := &TokenService{tokens: repo.NewTokenRepository(db), byteplus: &unknownBalanceClient{}, quotas: store}
	data, err := s.Quota(context.Background(), "byteplus", "a")
	if err != nil || store.calls != 0 || data["remaining"] != nil || data["unknown"] != true || data["error"] != "provider quota probe failed" {
		t.Fatalf("unknown probe overwrote quota or leaked diagnostics: data=%v calls=%d error=%v", data, store.calls, err)
	}
}
