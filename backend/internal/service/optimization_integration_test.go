package service

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"backend/internal/migrations"
	"backend/internal/model"
	"backend/internal/repo"
	"backend/internal/storage"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func optimizationTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("IMAGENEXA_TEST_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL test database")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("cannot connect to test database")
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	var name string
	if db.Raw("SELECT current_database()").Scan(&name).Error != nil || !strings.HasPrefix(name, "imagenexa_optimization_test_") {
		t.Fatal("refusing non-test database")
	}
	// Historical migrations extend operational tables from the pre-migration
	// installation. Recreate that baseline, then apply every real SQL constraint.
	if db.Exec(`CREATE TABLE IF NOT EXISTS refresh_profiles (
		id VARCHAR(64) PRIMARY KEY, pool VARCHAR(64) NOT NULL, kind VARCHAR(64) NOT NULL
	)`).Error != nil {
		t.Fatal("create legacy refresh profile baseline")
	}
	if db.AutoMigrate(&model.EventLog{}) != nil {
		t.Fatal("create operational event baseline")
	}
	if err := migrations.Run(context.Background(), db); err != nil {
		t.Fatal("test migrations failed:", err)
	}
	if db.AutoMigrate(model.AutoMigrateModels()...) != nil {
		t.Fatal("test operational schema creation failed")
	}
	tx := db.Begin()
	t.Cleanup(func() { tx.Rollback() })
	if tx.Error != nil {
		t.Fatal("begin test transaction")
	}
	return tx
}

func TestInterruptedTaskRecoveryIntegration(t *testing.T) {
	db := optimizationTestDB(t)
	ctx := context.Background()
	var pngData bytes.Buffer
	_ = png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/exists.png") {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngData.Bytes())
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	models := repo.NewModelRepository(db)
	events := repo.NewEventRepository(db)
	inflight := &InflightRegistry{}
	m := &MaintenanceService{models: models, events: events, inflight: inflight, recovery: &V1Service{}, store: storage.New(server.URL, "test", "test", "test")}
	if db.Create(&model.TokenAccount{ID: "test-account", Pool: "chatgpt", Status: "active", Value: "test"}).Error != nil {
		t.Fatal("create test account")
	}
	for _, tc := range []struct {
		id, file, status, state string
		live                    bool
	}{
		{"exists", "exists.png", "success", "succeeded", false},
		{"missing", "missing.png", "failed", "unresolved", false},
		{"video", "", "failed", "unresolved", false},
		{"live", "", "pending", "submitting", true},
	} {
		event := model.EventLog{ID: tc.id, Status: "pending", Kind: "image", Provider: "chatgpt", TS: time.Now().Add(-25 * 24 * time.Hour), File: tc.file}
		if tc.id == "video" {
			event.Kind = "video"
			event.Provider = "grok"
		}
		if events.Create(ctx, &event) != nil {
			t.Fatal("create event")
		}
		attempt, err := models.Dispatch().Start(ctx, tc.id, "image.gpt-image-2.chatgpt", "test-account")
		if err != nil {
			t.Fatal("start dispatch")
		}
		if db.Model(&model.DispatchAttempt{}).Where("id=?", attempt.ID).Update("started_at", event.TS).Error != nil {
			t.Fatal("age dispatch")
		}
		if tc.live {
			inflight.Add(tc.id, func() {})
		}
	}
	m.reconcileInterruptedDispatches(ctx)
	for _, tc := range []struct{ id, status, state string }{{"exists", "success", "succeeded"}, {"missing", "failed", "unresolved"}, {"video", "failed", "unresolved"}, {"live", "pending", "submitting"}} {
		event, err := events.GetByID(ctx, tc.id)
		if err != nil || event.Status != tc.status {
			t.Fatalf("%s status: %#v", tc.id, event)
		}
		var attempt model.DispatchAttempt
		db.Where("event_id=?", tc.id).First(&attempt)
		if attempt.State != tc.state {
			t.Fatalf("%s dispatch=%s", tc.id, attempt.State)
		}
		if tc.status == "failed" && (event.ElapsedMS <= 0 || event.Phase != "outcome_unknown" || event.Error != ErrGenerationOutcomeUnknown.Error()) {
			t.Fatal("missing failure diagnostics")
		}
	}
	m.reconcileInterruptedDispatches(ctx) // terminal decisions are idempotent
}

func TestQuotaAndAcceptedHandleIntegration(t *testing.T) {
	db := optimizationTestDB(t)
	ctx := context.Background()
	models := repo.NewModelRepository(db)
	account := model.TokenAccount{ID: "account", Pool: "chatgpt", Status: "active", Value: "test"}
	if db.Create(&account).Error != nil {
		t.Fatal("create account")
	}
	q := models.Quotas()
	if q.EnsureProviderBuckets(ctx, "chatgpt", "chatgpt.image", "generations") != nil {
		t.Fatal("seed quota")
	}
	bucket, err := q.Get(ctx, account.ID, "chatgpt.image")
	if err != nil || bucket.Remaining != nil {
		t.Fatal("unknown was invented as zero")
	}
	due, err := q.ClaimRefreshDue(ctx, time.Now(), 20)
	if err != nil || len(due) != 1 {
		t.Fatal("missing bucket not eligible for refresh")
	}
	event := model.EventLog{ID: "event", Kind: "image", Status: "pending", TS: time.Now()}
	if db.Create(&event).Error != nil {
		t.Fatal("create event")
	}
	attempt, err := models.Dispatch().Start(ctx, event.ID, "image.gpt-image-2.chatgpt", account.ID)
	if err != nil {
		t.Fatal("start")
	}
	if models.Dispatch().AcceptLatest(ctx, event.ID, account.ID, attempt.ModelRouteID, "existing-conversation") != nil {
		t.Fatal("accept")
	}
	if models.Dispatch().Finish(ctx, attempt.ID, "succeeded", "", "", nil) != nil {
		t.Fatal("finish")
	}
	saved, err := models.Dispatch().LatestAcceptedForEvent(ctx, event.ID)
	if err != nil || saved.UpstreamTaskID != "existing-conversation" {
		t.Fatal("success erased recovery handle")
	}
	two, one := 2.0, 1.0
	if _, err = q.UpsertSnapshot(ctx, account.ID, "chatgpt.image", "generations", nil, &two, nil); err != nil {
		t.Fatal("snapshot")
	}
	reservation, err := q.ReserveWithUnit(ctx, event.ID, attempt.ID, account.ID, "chatgpt.image", "generations", 1)
	if err != nil {
		t.Fatal("reserve")
	}
	if _, err = q.UpsertSnapshot(ctx, account.ID, "chatgpt.image", "generations", nil, &one, nil); err != nil {
		t.Fatal("post-generation snapshot")
	}
	for range 2 {
		if q.Settle(ctx, reservation.ID, &one) != nil {
			t.Fatal("settle")
		}
	}
	bucket, err = q.Get(ctx, account.ID, "chatgpt.image")
	if err != nil || bucket.Remaining == nil || *bucket.Remaining != 1 || bucket.Reserved != 0 {
		t.Fatal("double quota debit/refund")
	}
	svc := NewAdminConsoleService(nil, db, models, repo.NewTokenRepository(db), nil, nil)
	counts, err := svc.CountAccountHealthByProvider(ctx)
	if err != nil || counts["chatgpt"]["quota_available"] != 1 {
		t.Fatal("quota summary failed")
	}
}
