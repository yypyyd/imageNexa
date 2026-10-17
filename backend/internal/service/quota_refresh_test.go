package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"backend/internal/model"
	"backend/internal/provider/byteplus"
	"backend/internal/repo"
)

func TestQuotaRefreshDispatchPolicy(t *testing.T) {
	known := dispatchPlan{QuotaTracked: true, Cost: 1}
	held := &model.QuotaReservation{Amount: 1, BalanceKnown: true}
	if shouldRefreshDispatchQuota(known, "chatgpt", held, nil) {
		t.Fatal("known cost triggered a second provider request")
	}
	if !shouldRefreshDispatchQuota(dispatchPlan{}, "grok", nil, nil) {
		t.Fatal("unknown cost was not reconciled")
	}
	if !shouldRefreshDispatchQuota(known, "chatgpt", &model.QuotaReservation{Amount: 1}, nil) {
		t.Fatal("unknown initial balance never reconciled")
	}
	if shouldRefreshDispatchQuota(known, "dola", held, nil) {
		t.Fatal("Dola daily allowance made a quota request")
	}
	proof := verifiedBytePlusRetry(byteplus.ErrRetryableTaskFailed, 100)
	if !shouldRefreshDispatchQuota(known, "byteplus", held, proof) {
		t.Fatal("billing proof was skipped")
	}
}

func TestDailyQuotaRefreshIntegration(t *testing.T) {
	db := optimizationTestDB(t)
	ctx := context.Background()
	account := model.TokenAccount{ID: "daily-account", Pool: "chatgpt", Status: "active", Value: "test"}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	q := repo.NewQuotaRepository(db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	claim, err := q.ClaimProbe(ctx, account.ID, "chatgpt.image", "generations", repo.QuotaProbeManual, now)
	if err != nil || claim == nil {
		t.Fatal("claim", err)
	}
	two := 2.0
	if ok, err := q.FinishProbe(ctx, *claim, nil, &two, nil, now); err != nil || !ok {
		t.Fatal("finish", err)
	}
	// Local account/ledger activity must not turn the 24-hour deadline into a
	// moving target, nor should it cause a 15-minute background query.
	if err := db.Model(&model.AccountQuotaBucket{}).Where("id=?", claim.ID).UpdateColumn("updated_at", now.Add(23*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if due, err := q.ClaimRefreshDue(ctx, now.Add(23*time.Hour), 20); err != nil || len(due) != 0 {
		t.Fatal("queried early", err, len(due))
	}
	due, err := q.ClaimRefreshDue(ctx, now.Add(24*time.Hour), 20)
	if err != nil || len(due) != 1 {
		t.Fatal("daily refresh missing", err, len(due))
	}
	if again, err := q.ClaimRefreshDue(ctx, now.Add(24*time.Hour), 20); err != nil || len(again) != 0 {
		t.Fatal("duplicate worker", err)
	}
	probe := due[0]
	for _, delay := range []time.Duration{15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour} {
		finished := *probe.LastProbeAt
		if _, err := q.FinishProbe(ctx, probe, nil, nil, nil, finished); err != nil {
			t.Fatal(err)
		}
		bucket, err := q.Get(ctx, account.ID, "chatgpt.image")
		if err != nil || bucket.Remaining == nil || *bucket.Remaining != 2 || !bucket.RefreshedAt.Equal(now) || !bucket.RefreshAfter.Equal(finished.Add(delay)) {
			t.Fatalf("failed probe changed balance or retry schedule: %+v %v", bucket, err)
		}
		if err := db.Model(&model.AccountQuotaBucket{}).Where("id=?", bucket.ID).UpdateColumn("reserved", 1).Error; err != nil {
			t.Fatal(err)
		}
		if claimed, err := q.ClaimProbe(ctx, account.ID, "chatgpt.image", "generations", repo.QuotaProbeUsage, finished.Add(time.Second)); err != nil || claimed != nil {
			t.Fatal("generation bypassed failure backoff", err)
		}
		check, _ := q.Get(ctx, account.ID, "chatgpt.image")
		if !check.RefreshAfter.Equal(*bucket.RefreshAfter) {
			t.Fatal("in-flight work shortened failure backoff")
		}
		if err := db.Model(&model.AccountQuotaBucket{}).Where("id=?", bucket.ID).UpdateColumn("reserved", 0).Error; err != nil {
			t.Fatal(err)
		}
		if items, err := q.ClaimRefreshDue(ctx, finished.Add(delay-time.Second), 20); err != nil || len(items) != 0 {
			t.Fatal("backoff bypassed", err)
		}
		due, err = q.ClaimRefreshDue(ctx, finished.Add(delay), 20)
		if err != nil || len(due) != 1 {
			t.Fatal("retry missing", err)
		}
		probe = due[0]
	}
	if _, err = q.FinishProbe(ctx, probe, nil, &two, nil, *probe.LastProbeAt); err != nil {
		t.Fatal(err)
	}
	bucket, _ := q.Get(ctx, account.ID, "chatgpt.image")
	if bucket.RefreshFailures != 0 || !bucket.RefreshAfter.Equal(probe.LastProbeAt.Add(24*time.Hour)) {
		t.Fatal("success did not reset backoff")
	}
}

func TestQuotaRefreshRecoveryAndLeaseIntegration(t *testing.T) {
	db := optimizationTestDB(t)
	ctx := context.Background()
	q := repo.NewQuotaRepository(db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	zero, two := 0.0, 2.0
	for _, pool := range []string{"chatgpt", "runway", "oreate"} {
		a := model.TokenAccount{ID: pool, Pool: pool, Status: "active", Value: "test"}
		if err := db.Create(&a).Error; err != nil {
			t.Fatal(err)
		}
		key, unit, _ := providerSnapshotScope(pool)
		claim, err := q.ClaimProbe(ctx, a.ID, key, unit, repo.QuotaProbeManual, now)
		if err != nil || claim == nil {
			t.Fatal(err)
		}
		reset := now.Add(time.Hour)
		if _, err := q.FinishProbe(ctx, *claim, nil, &zero, &reset, now); err != nil {
			t.Fatal(err)
		}
	}
	due, err := q.ClaimRefreshDue(ctx, now.Add(time.Hour), 20)
	if err != nil || len(due) != 1 || due[0].AccountID != "chatgpt" {
		t.Fatal("reset/expiration confused", err, due)
	}
	old := due[0]
	if busy, err := q.ClaimProbe(ctx, "chatgpt", "chatgpt.image", "generations", repo.QuotaProbeUsage, now.Add(time.Hour+time.Second)); err != nil || busy != nil {
		t.Fatal("concurrent probe not merged", err)
	}
	newClaim, err := q.ClaimProbe(ctx, "chatgpt", "chatgpt.image", "generations", repo.QuotaProbeManual, now.Add(time.Hour+6*time.Minute))
	if err != nil || newClaim == nil {
		t.Fatal("expired lease stranded refresh", err)
	}
	if ok, err := q.FinishProbe(ctx, old, nil, &two, nil, now.Add(time.Hour+6*time.Minute)); err != nil || ok {
		t.Fatal("old lease overwrote newer worker", err)
	}
	if ok, err := q.FinishProbe(ctx, *newClaim, nil, &two, nil, now.Add(time.Hour+6*time.Minute)); err != nil || !ok {
		t.Fatal("new lease failed", err)
	}
}

func TestQuotaRefreshCoordinatorIntegration(t *testing.T) {
	db := optimizationTestDB(t)
	ctx := context.Background()
	q := repo.NewQuotaRepository(db)
	a := model.TokenAccount{ID: "coordinator", Pool: "grok", Status: "quota", Value: "test"}
	if err := db.Create(&a).Error; err != nil {
		t.Fatal(err)
	}
	calls := 0
	fetch := func(context.Context) (map[string]any, error) { calls++; return map[string]any{"remaining": 10}, nil }
	for range 2 {
		if _, err := runQuotaRefresh(ctx, q, "grok", a.ID, repo.QuotaProbeManual, nil, fetch); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatal("manual refresh sent duplicate requests")
	}
	loaded, loadErr := repo.NewTokenRepository(db).Get(ctx, a.Pool, a.ID)
	if loadErr != nil || loaded.Status != "active" {
		t.Fatal("verified positive quota did not restore account", loadErr)
	}
	// A generation completed after this snapshot. Coalescing it must queue a
	// follow-up rather than declaring the old snapshot current for another day.
	if _, err := runQuotaRefresh(ctx, q, "grok", a.ID, repo.QuotaProbeUsage, nil, fetch); err != nil {
		t.Fatal(err)
	}
	bucket, _ := q.Get(ctx, a.ID, "grok.media")
	if bucket.RefreshAfter.After(time.Now().Add(2 * time.Minute)) {
		t.Fatal("coalesced generation lost its refresh")
	}
	if _, err := runQuotaRefresh(ctx, q, "grok", a.ID, repo.QuotaProbeVerification, nil, func(context.Context) (map[string]any, error) { return nil, errors.New("network failure") }); err == nil {
		t.Fatal("lost provider failure")
	}
	bucket, _ = q.Get(ctx, a.ID, "grok.media")
	if bucket.Remaining == nil || *bucket.Remaining != 10 || bucket.RefreshFailures != 1 {
		t.Fatal("failure discarded last balance")
	}
}

func TestLocalQuotaLedgerMirrorIntegration(t *testing.T) {
	db := optimizationTestDB(t)
	ctx := context.Background()
	q := repo.NewQuotaRepository(db)
	a := model.TokenAccount{ID: "local-ledger", Pool: "chatgpt", Status: "active", Value: "test"}
	if err := db.Create(&a).Error; err != nil {
		t.Fatal(err)
	}
	two := 2.0
	if _, err := q.UpsertSnapshot(ctx, a.ID, "chatgpt.image", "generations", &two, &two, nil); err != nil {
		t.Fatal(err)
	}
	event := model.EventLog{ID: "local-event", Kind: "image", Status: "pending", TS: time.Now()}
	if err := db.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	models := repo.NewModelRepository(db)
	attempt, err := models.Dispatch().Start(ctx, event.ID, "image.gpt-image-2.chatgpt", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Start an automatic observation, then let a generation race it. Applying
	// the returned upstream balance would otherwise debit that generation twice.
	probeAt := time.Now().Add(2 * time.Minute)
	probe, err := q.ClaimProbe(ctx, a.ID, "chatgpt.image", "generations", repo.QuotaProbeManual, probeAt)
	if err != nil || probe == nil {
		t.Fatal(err)
	}
	r1, err := q.ReserveWithUnit(ctx, event.ID, attempt.ID, a.ID, "chatgpt.image", "generations", 1)
	if err != nil {
		t.Fatal(err)
	}
	one := 1.0
	if applied, err := q.FinishProbe(ctx, *probe, nil, &one, nil, probeAt); err != nil || applied {
		t.Fatal("snapshot racing a reservation was trusted", err)
	}
	if due, err := q.ClaimRefreshDue(ctx, probeAt.Add(24*time.Hour), 20); err != nil || len(due) != 0 {
		t.Fatal("background query ran during a reservation", err)
	}
	r2, err := q.ReserveWithUnit(ctx, event.ID, attempt.ID, a.ID, "chatgpt.image", "generations", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.ReserveWithUnit(ctx, event.ID, attempt.ID, a.ID, "chatgpt.image", "generations", 1); !errors.Is(err, repo.ErrQuotaUnavailable) {
		t.Fatal("overspent ledger", err)
	}
	if err := q.Settle(ctx, r1.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := q.Release(ctx, r2.ID); err != nil {
		t.Fatal(err)
	}
	if err := q.Release(ctx, r2.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err := repo.NewTokenRepository(db).Get(ctx, a.Pool, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	remaining, known := jsonMapFloat(loaded.Meta, "cached_quota_remaining")
	if !known || remaining != 1 {
		t.Fatal("adapter metadata diverged from ledger", remaining)
	}
	bucket, _ := q.Get(ctx, a.ID, "chatgpt.image")
	if *bucket.Remaining != 1 || bucket.Reserved != 0 {
		t.Fatal("settlement double counted")
	}
}
