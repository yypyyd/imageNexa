package service

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"backend/internal/model"
	"backend/internal/repo"
	"gorm.io/gorm"
)

func unknownSubmissionFixture(t *testing.T, db *gorm.DB) (model.DispatchAttempt, model.QuotaReservation) {
	t.Helper()
	ctx := context.Background()
	models := repo.NewModelRepository(db)
	account := model.TokenAccount{ID: "unknown-account", Pool: "byteplus", Status: "active", Value: "test"}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	balance := 100.0
	if _, err := models.Quotas().UpsertSnapshot(ctx, account.ID, "byteplus.computing_points", "points", nil, &balance, nil); err != nil {
		t.Fatal(err)
	}
	event := model.EventLog{ID: "unknown-event", Kind: "image", Status: "pending", Provider: "byteplus", Model: "byteplus-gpt-image-2", TS: time.Now().Add(-2 * time.Hour)}
	if err := db.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	attempt, err := models.Dispatch().Start(ctx, event.ID, "image.gpt-image-2.byteplus", account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := models.Dispatch().Finish(ctx, attempt.ID, "unknown", "temporary", "", nil); err != nil {
		t.Fatal(err)
	}
	attempt.StartedAt = event.TS
	if err := db.Model(attempt).Updates(map[string]any{"started_at": event.TS, "finished_at": event.TS}).Error; err != nil {
		t.Fatal(err)
	}
	reservation, err := models.Quotas().ReserveWithUnit(ctx, event.ID, attempt.ID, account.ID, "byteplus.computing_points", "points", 10)
	if err != nil {
		t.Fatal(err)
	}
	return *attempt, *reservation
}

func TestUnverifiedSubmissionTimeoutPreservesQuotaIntegration(t *testing.T) {
	db := optimizationTestDB(t)
	attempt, reservation := unknownSubmissionFixture(t, db)
	ctx := context.Background()
	models := repo.NewModelRepository(db)
	tokens := repo.NewTokenRepository(db)
	settings := repo.NewSiteSettingRepository(db, nil)
	m := &MaintenanceService{
		models: models, tokens: tokens, events: repo.NewEventRepository(db),
		tokenSvc: &TokenService{tokens: tokens, settings: settings}, recovery: &V1Service{settings: settings},
	}
	for range 2 {
		m.reconcileUnknownSubmissions(ctx)
		m.reconcileOpenQuotaReservations(ctx)
	}
	saved, err := models.Dispatch().Get(ctx, attempt.ID)
	if err != nil || saved.State != "unresolved" || saved.FailureClass != "outcome_unknown" {
		t.Fatalf("unverified submission lost uncertainty: attempt=%+v err=%v", saved, err)
	}
	var held model.QuotaReservation
	if err := db.First(&held, "id = ?", reservation.ID).Error; err != nil || held.Status != "uncertain" {
		t.Fatalf("unverified submission refunded: reservation=%+v err=%v", held, err)
	}
	bucket, err := models.Quotas().Get(ctx, attempt.AccountID, "byteplus.computing_points")
	if err != nil || bucket.Remaining == nil || *bucket.Remaining != 90 || bucket.Reserved != 10 {
		t.Fatalf("unverified submission invented quota: bucket=%+v err=%v", bucket, err)
	}
	event, err := m.events.GetByID(ctx, attempt.EventID)
	if err != nil || event.Status != "failed" || event.Phase != "outcome_unknown" || event.Error != ErrGenerationOutcomeUnknown.Error() {
		t.Fatalf("client did not receive a terminal uncertain outcome: event=%+v err=%v", event, err)
	}
}

func TestVerifiedAbsentSubmissionReleasesQuotaIntegration(t *testing.T) {
	db := optimizationTestDB(t)
	attempt, reservation := unknownSubmissionFixture(t, db)
	ctx := context.Background()
	models := repo.NewModelRepository(db)
	m := &MaintenanceService{models: models, tokens: repo.NewTokenRepository(db), tokenSvc: &TokenService{}}
	for range 2 {
		m.finishUnknownSubmission(ctx, attempt, unknownSubmissionAbsent)
		m.reconcileOpenQuotaReservations(ctx)
	}
	saved, err := models.Dispatch().Get(ctx, attempt.ID)
	if err != nil || saved.State != "failed" || saved.FailureClass != "temporary" {
		t.Fatalf("verified absence did not close dispatch: %+v err=%v", saved, err)
	}
	var held model.QuotaReservation
	if err := db.First(&held, "id = ?", reservation.ID).Error; err != nil || held.Status != "released" {
		t.Fatalf("verified absence did not release reservation: %+v err=%v", held, err)
	}
	bucket, err := models.Quotas().Get(ctx, attempt.AccountID, "byteplus.computing_points")
	if err != nil || bucket.Remaining == nil || *bucket.Remaining != 100 || bucket.Reserved != 0 {
		t.Fatalf("verified absence was refunded incorrectly: %+v err=%v", bucket, err)
	}
}

func TestUnknownResolutionPreservesConcurrentAcceptanceIntegration(t *testing.T) {
	db := optimizationTestDB(t)
	attempt, reservation := unknownSubmissionFixture(t, db)
	ctx := context.Background()
	dispatch := repo.NewModelRepository(db).Dispatch()
	if err := dispatch.Adopt(ctx, attempt.ID, "accepted-by-another-worker"); err != nil {
		t.Fatal(err)
	}
	for _, absent := range []bool{false, true} {
		applied, err := dispatch.ResolveUnknown(ctx, attempt, absent, ErrGenerationOutcomeUnknown.Error())
		if err != nil || applied {
			t.Fatalf("stale verdict overwrote accepted task: applied=%v err=%v", applied, err)
		}
	}
	saved, err := dispatch.Get(ctx, attempt.ID)
	if err != nil || saved.State != "accepted" || saved.UpstreamTaskID != "accepted-by-another-worker" {
		t.Fatalf("accepted handle lost: %+v err=%v", saved, err)
	}
	var held model.QuotaReservation
	if err := db.First(&held, "id = ?", reservation.ID).Error; err != nil || held.Status != "held" {
		t.Fatalf("stale verdict changed accepted reservation: %+v err=%v", held, err)
	}
	event, err := repo.NewEventRepository(db).GetByID(ctx, attempt.EventID)
	if err != nil || event.Status != "pending" {
		t.Fatal("stale verdict closed a recoverable event")
	}
}

func TestOverviewCountsEntireAccountPoolIntegration(t *testing.T) {
	db := optimizationTestDB(t)
	accounts := make([]model.TokenAccount, 0, 208)
	for i := range 205 {
		accounts = append(accounts, model.TokenAccount{ID: fmt.Sprintf("summary-account-%d", i), Pool: "chatgpt", Status: "active", Value: "test"})
	}
	accounts = append(accounts,
		model.TokenAccount{ID: "summary-disabled", Pool: "chatgpt", Status: "disabled", Value: "test"},
		model.TokenAccount{ID: "summary-dead", Pool: "chatgpt", Status: "active", Dead: true, Value: "test"},
		model.TokenAccount{ID: "summary-retired", Pool: "oreate", Status: "active", Value: "test"},
	)
	if err := db.CreateInBatches(&accounts, 100).Error; err != nil {
		t.Fatal(err)
	}
	keys := make([]model.APICredential, 0, 206)
	for i := range 205 {
		keys = append(keys, model.APICredential{
			ID: fmt.Sprintf("summary-key-%d", i), Name: "test", KeyHash: fmt.Sprintf("%064x", i+1), Status: model.APICredentialStatusActive,
		})
	}
	keys = append(keys, model.APICredential{ID: "summary-key-disabled", Name: "test", KeyHash: fmt.Sprintf("%064x", 206), Status: model.APICredentialStatusDisabled})
	if err := db.CreateInBatches(&keys, 100).Error; err != nil {
		t.Fatal(err)
	}
	svc := NewAdminConsoleService(nil, db, repo.NewModelRepository(db), repo.NewTokenRepository(db), nil, nil)
	overview, err := svc.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(overview["totals"])
	if err != nil {
		t.Fatal(err)
	}
	var totals map[string]int64
	if err := json.Unmarshal(raw, &totals); err != nil || totals["accounts"] != 205 || totals["keys"] != 205 {
		t.Fatalf("overview truncated or misclassified totals: %s, err=%v", raw, err)
	}
}
