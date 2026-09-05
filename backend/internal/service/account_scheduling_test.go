package service

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"backend/internal/model"
	"backend/internal/provider/adobe"
	"backend/internal/provider/byteplus"
	"backend/internal/repo"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func schedulingDB(t *testing.T) (*gorm.DB, sqlmock.Sqlmock) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB, PreferSimpleProtocol: true}), &gorm.Config{
		DisableAutomaticPing: true, SkipDefaultTransaction: true, Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		_ = sqlDB.Close()
	})
	return db, mock
}

func schedulingRows(accounts ...model.TokenAccount) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"id", "pool", "status", "dead", "value", "concurrency"})
	for _, account := range accounts {
		rows.AddRow(account.ID, account.Pool, account.Status, account.Dead, account.Value, account.Concurrency)
	}
	return rows
}

func schedulingAccount(id string) model.TokenAccount {
	return model.TokenAccount{ID: id, Pool: "adobe", Status: "active", Value: "test-credential-" + id, Concurrency: 1}
}

func TestQueuedAccountRefreshPreservesOrderAndCurrentCredential(t *testing.T) {
	db, mock := schedulingDB(t)
	a, b, c := schedulingAccount("a"), schedulingAccount("b"), schedulingAccount("c")
	active := []model.TokenAccount{b, c, a}
	b.Status = "disabled"
	a.Value = "rotated-credential"
	mock.ExpectQuery(`SELECT .* FROM "provider_accounts"`).WillReturnRows(schedulingRows(a, b, c))
	s := &V1Service{tokens: repo.NewTokenRepository(db)}
	fresh, err := s.refreshPoolAccounts(context.Background(), "adobe", "image", active, nil)
	if err != nil || len(fresh) != 2 || fresh[0].ID != "c" || fresh[1].ID != "a" || fresh[1].Value != a.Value {
		t.Fatalf("refreshed candidates = %#v, %v", fresh, err)
	}
}

type acquisitionCounter struct{ count atomic.Int32 }

func (*acquisitionCounter) DialHook(next redis.DialHook) redis.DialHook { return next }
func (*acquisitionCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *acquisitionCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "evalsha" {
			h.count.Add(1)
		}
		return next(ctx, cmd)
	}
}

func TestBusyPoolUsesCapacityBatchBeforeAcquisition(t *testing.T) {
	db, mock := schedulingDB(t)
	gate, _ := testConcurrency(t)
	accounts := make([]model.TokenAccount, 100)
	for i := range accounts {
		accounts[i] = schedulingAccount(fmt.Sprint(i))
		if ok, err := gate.Acquire(context.Background(), "conc:a:"+accounts[i].ID, 1, "occupied"); err != nil || !ok {
			t.Fatal(err)
		}
	}
	hook := &acquisitionCounter{}
	gate.redis.AddHook(hook)
	mock.ExpectQuery(`SELECT .* FROM "provider_accounts"`).WillReturnRows(schedulingRows(accounts...))
	s := &V1Service{tokens: repo.NewTokenRepository(db), conc: gate}
	ctx := context.WithValue(context.Background(), poolSchedulingKey, poolSchedulingPolicy{fastFailover: true})
	_, err := s.runPoolWithFailover(ctx, "queued", "adobe", accounts, "image", func(model.TokenAccount) ([]byte, error) {
		t.Fatal("busy account reached provider submission")
		return nil, nil
	}, adobeErrClass, nil, true)
	if !errors.Is(err, ErrConcurrencyFull) || hook.count.Load() != 0 {
		t.Fatalf("full pool error=%v, per-account acquisition calls=%d", err, hook.count.Load())
	}
}

func TestAccountDisabledAfterQueueSnapshotIsNotSubmitted(t *testing.T) {
	db, mock := schedulingDB(t)
	gate, _ := testConcurrency(t)
	account := schedulingAccount("a")
	mock.ExpectQuery(`SELECT .* FROM "provider_accounts"`).WillReturnRows(schedulingRows(account))
	account.Status = "disabled"
	mock.ExpectQuery(`SELECT .* FROM "provider_accounts"`).WillReturnRows(schedulingRows(account))
	s := &V1Service{tokens: repo.NewTokenRepository(db), conc: gate}
	_, err := s.runPoolWithFailover(context.Background(), "queued", "adobe", []model.TokenAccount{account}, "image", func(model.TokenAccount) ([]byte, error) {
		t.Fatal("disabled account reached provider submission")
		return nil, nil
	}, adobeErrClass, nil, true)
	if !errors.Is(err, ErrNoProviderAccount) {
		t.Fatalf("error = %v", err)
	}
	if count := gate.ActiveCount(context.Background(), "conc:a:a"); count != 0 {
		t.Fatalf("rejected account retained %d slots", count)
	}
}

func TestRouteRankingRetainsRoundRobinForEquivalentAccounts(t *testing.T) {
	db, mock := schedulingDB(t)
	s := &V1Service{models: repo.NewModelRepository(db)}
	route := model.ModelRoute{ID: "test-route", Provider: "custom"}
	var heads []string
	for iteration := 0; iteration < 2; iteration++ {
		mock.ExpectQuery(`SELECT count`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
		bindings := sqlmock.NewRows([]string{"account_id", "model_route_id", "enabled", "entitled"})
		for _, id := range []string{"a", "b", "c"} {
			bindings.AddRow(id, route.ID, true, true)
		}
		mock.ExpectQuery(`SELECT .* FROM "account_model_routes"`).WillReturnRows(bindings)
		accounts := poolAccounts("a", "b", "c")
		s.rotateRoundRobin("custom", accounts)
		wantHead := accounts[0].ID
		ranked, err := s.routeAccounts(context.Background(), route, accounts)
		if err != nil || len(ranked) != 3 {
			t.Fatalf("routeAccounts = %v, %v", ranked, err)
		}
		if ranked[0].ID != wantHead {
			t.Fatalf("ranking replaced rotated head %s with %s", wantHead, ranked[0].ID)
		}
		heads = append(heads, ranked[0].ID)
	}
	if heads[0] == heads[1] {
		t.Fatal("equivalent requests selected the same head")
	}
}

func TestMediaRoutingTriesSpareCapacityBeforeWaiting(t *testing.T) {
	var calls []string
	started := time.Now()
	_, url, err := runMediaRouteFailover(context.Background(), []model.ModelRoute{{ID: "busy"}, {ID: "free"}}, "", func(ctx context.Context, route model.ModelRoute) ([]byte, string, error) {
		if !poolPolicy(ctx).fastFailover {
			t.Fatal("multi-route attempt retained the per-pool wait")
		}
		calls = append(calls, route.ID)
		if route.ID == "busy" {
			return nil, "", ErrConcurrencyFull
		}
		return nil, "result", nil
	})
	if err != nil || url != "result" || len(calls) != 2 || time.Since(started) > time.Second {
		t.Fatalf("routing calls=%v, url=%s, error=%v", calls, url, err)
	}
}

func TestMediaRoutingRevisitsOnlyBusyRoutes(t *testing.T) {
	calls := map[string]int{}
	_, _, err := runMediaRouteFailover(context.Background(), []model.ModelRoute{{ID: "failed"}, {ID: "busy"}}, "", func(ctx context.Context, route model.ModelRoute) ([]byte, string, error) {
		calls[route.ID]++
		if route.ID == "failed" {
			return nil, "", adobe.ErrAuth
		}
		if calls[route.ID] == 1 {
			return nil, "", ErrConcurrencyFull
		}
		return []byte("image"), "", nil
	})
	if err != nil || calls["failed"] != 1 || calls["busy"] != 2 {
		t.Fatalf("calls=%v, error=%v", calls, err)
	}
}

func TestMediaRoutingDoesNotResubmitAcceptedTask(t *testing.T) {
	calls := 0
	_, _, err := runMediaRouteFailover(context.Background(), []model.ModelRoute{{ID: "accepted"}, {ID: "spare"}}, "", func(context.Context, model.ModelRoute) ([]byte, string, error) {
		calls++
		return nil, "", errors.Join(byteplus.ErrTaskAccepted, byteplus.ErrTemporaryUpstream)
	})
	if !errors.Is(err, byteplus.ErrTaskAccepted) || calls != 1 {
		t.Fatalf("accepted task retried: calls=%d error=%v", calls, err)
	}
}

func TestMediaQueueHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, err := runMediaRouteFailover(ctx, []model.ModelRoute{{ID: "a"}, {ID: "b"}}, "", func(context.Context, model.ModelRoute) ([]byte, string, error) {
		return nil, "", ErrConcurrencyFull
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
}

func expectSchedulingAttempt(mock sqlmock.Sqlmock, account model.TokenAccount) {
	mock.ExpectQuery(`SELECT .* FROM "provider_accounts"`).WillReturnRows(schedulingRows(account))
	mock.ExpectExec(`UPDATE "event_logs"`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE "provider_accounts"`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE "provider_accounts"`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT .* FROM "provider_accounts"`).WillReturnRows(schedulingRows(account))
}

func TestTemporaryRetryDoesNotRevisitAnAuthFailedAccount(t *testing.T) {
	db, mock := schedulingDB(t)
	gate, _ := testConcurrency(t)
	a, b := schedulingAccount("a"), schedulingAccount("b")
	mock.ExpectQuery(`SELECT .* FROM "provider_accounts"`).WillReturnRows(schedulingRows(a, b))
	expectSchedulingAttempt(mock, a)
	expectSchedulingAttempt(mock, b)
	// Even if the account's provider leaves auth failures active, it must be
	// excluded from this request. The second refresh asks only for B.
	mock.ExpectQuery(`SELECT .* FROM "provider_accounts"`).WithArgs("adobe", "b").WillReturnRows(schedulingRows(b))
	expectSchedulingAttempt(mock, b)
	s := &V1Service{tokens: repo.NewTokenRepository(db), events: repo.NewEventRepository(db), conc: gate}
	calls := map[string]int{}
	data, err := s.runPoolWithFailover(context.Background(), "event", "adobe", []model.TokenAccount{a, b}, "image", func(account model.TokenAccount) ([]byte, error) {
		calls[account.ID]++
		if account.ID == "a" {
			return nil, adobe.ErrAuth
		}
		if calls[account.ID] == 1 {
			return nil, adobe.ErrTemporaryUpstream
		}
		return []byte("success"), nil
	}, adobeErrClass, nil, true)
	if err != nil || string(data) != "success" || calls["a"] != 1 || calls["b"] != 2 {
		t.Fatalf("retry revisited failed account: calls=%v data=%q error=%v", calls, data, err)
	}
}

func TestFirstAdmissionRespectsCooldownButExistingRetryCanContinue(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprint(retry), func(t *testing.T) {
			db, mock := schedulingDB(t)
			account := schedulingAccount("a")
			mock.ExpectQuery(`SELECT .* FROM "provider_accounts"`).WillReturnRows(schedulingRows(account))
			mock.ExpectQuery(`SELECT account_model_routes.*JOIN model_routes.*JOIN logical_models`).
				WillReturnRows(sqlmock.NewRows([]string{"account_id", "model_route_id", "enabled", "entitled", "cooldown_until"}).
					AddRow("a", "route", true, true, time.Now().Add(time.Minute)))
			s := &V1Service{tokens: repo.NewTokenRepository(db), models: repo.NewModelRepository(db)}
			ctx := withDispatchRoute(context.Background(), model.ModelRoute{ID: "route", Provider: "adobe"}, 0)
			_, err := s.revalidateDispatchAccount(ctx, "adobe", "a", "image", retry)
			if retry && err != nil || !retry && !errors.Is(err, ErrNoProviderAccount) {
				t.Fatalf("retry=%v error=%v", retry, err)
			}
		})
	}
}

func TestDispatchRejectsDisabledModelOrRoute(t *testing.T) {
	db, mock := schedulingDB(t)
	account := schedulingAccount("a")
	mock.ExpectQuery(`SELECT .* FROM "provider_accounts"`).WillReturnRows(schedulingRows(account))
	mock.ExpectQuery(`SELECT account_model_routes.*JOIN model_routes.*JOIN logical_models.*model_routes.enabled.*logical_models.enabled`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	s := &V1Service{tokens: repo.NewTokenRepository(db), models: repo.NewModelRepository(db)}
	ctx := withDispatchRoute(context.Background(), model.ModelRoute{ID: "route", Provider: "adobe"}, 0)
	if _, err := s.revalidateDispatchAccount(ctx, "adobe", "a", "image", true); !errors.Is(err, ErrNoProviderAccount) {
		t.Fatalf("disabled route/model admitted: %v", err)
	}
}

func TestAuthExclusionSurvivesRouteCapacityRequeue(t *testing.T) {
	db, mock := schedulingDB(t)
	gate, _ := testConcurrency(t)
	a, b := schedulingAccount("a"), schedulingAccount("b")
	if ok, err := gate.Acquire(context.Background(), "conc:a:b", 1, "occupied"); err != nil || !ok {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT .* FROM "provider_accounts"`).WillReturnRows(schedulingRows(a, b))
	expectSchedulingAttempt(mock, a)
	mock.ExpectQuery(`SELECT .* FROM "provider_accounts"`).WithArgs("adobe", "b").WillReturnRows(schedulingRows(b))
	expectSchedulingAttempt(mock, b)
	s := &V1Service{tokens: repo.NewTokenRepository(db), events: repo.NewEventRepository(db), conc: gate}
	visits := 0
	calls := map[string]int{}
	data, _, err := runMediaRouteFailover(context.Background(), []model.ModelRoute{{ID: "main"}, {ID: "empty"}}, "", func(ctx context.Context, route model.ModelRoute) ([]byte, string, error) {
		if route.ID == "empty" {
			return nil, "", ErrNoProviderAccount
		}
		visits++
		if visits == 2 {
			gate.Release(ctx, "conc:a:b", "occupied")
		}
		data, err := s.runPoolWithFailover(ctx, "event", "adobe", []model.TokenAccount{a, b}, "image", func(account model.TokenAccount) ([]byte, error) {
			calls[account.ID]++
			if account.ID == "a" {
				return nil, adobe.ErrAuth
			}
			return []byte("success"), nil
		}, adobeErrClass, nil, true)
		return data, "", err
	})
	if err != nil || string(data) != "success" || calls["a"] != 1 || calls["b"] != 1 || visits != 2 {
		t.Fatalf("capacity requeue lost exclusions: visits=%d calls=%v data=%q error=%v", visits, calls, data, err)
	}
}
