package service

import (
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"backend/internal/model"
	"backend/internal/repo"
	"backend/internal/storage"
)

// MaintenanceService runs the periodic self-healing sweep that the Python
// original did via a 60s daemon thread plus read-time lazy cleanup. Without it
// the Go token pool only ever loses capacity: tokens never re-activate after a
// quota reset, cookies never auto-renew, stale pending events permanently block
// a user's generation gate, and old media/logs accumulate unbounded.
type MaintenanceService struct {
	tokens          *repo.TokenRepository
	tokenSvc        *TokenService
	events          *repo.EventRepository
	refresh         *RefreshProfileService
	settings        *repo.SiteSettingRepository
	models          *repo.ModelRepository
	store           *storage.Client
	inflight        *InflightRegistry
	recovery        *V1Service
	interval        time.Duration
	stalePending    time.Duration
	mediaPruneEvery time.Duration
	lastMediaPrune  time.Time
	quotaRecoverAge time.Duration
}

func NewMaintenanceService(tokens *repo.TokenRepository, tokenSvc *TokenService, events *repo.EventRepository, refresh *RefreshProfileService, settings *repo.SiteSettingRepository, models *repo.ModelRepository, store *storage.Client, recovery *V1Service) *MaintenanceService {
	return &MaintenanceService{
		tokens:          tokens,
		tokenSvc:        tokenSvc,
		events:          events,
		refresh:         refresh,
		settings:        settings,
		models:          models,
		store:           store,
		inflight:        recovery.Inflight(),
		recovery:        recovery,
		interval:        60 * time.Second,
		stalePending:    600 * time.Second,
		mediaPruneEvery: 60 * time.Second,
		quotaRecoverAge: 2 * time.Minute,
	}
}

// Run drives the sweep every interval until ctx is cancelled. It runs one sweep
// immediately on startup so a freshly restarted process heals stuck state right
// away rather than after the first tick.
func (m *MaintenanceService) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	m.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.tick(ctx)
		}
	}
}

// syncRecoveredQuota re-probes recovered ChatGPT accounts so their displayed
// image balance reflects the post-reset value. Bounded concurrency avoids a
// thundering herd at the daily reset.
func (m *MaintenanceService) syncRecoveredQuota(accs []model.TokenAccount) {
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, acc := range accs {
		switch acc.Pool {
		case "chatgpt":
		default:
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(a model.TokenAccount) {
			defer wg.Done()
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			_, _ = m.tokenSvc.Quota(ctx, a.Pool, a.ID)
		}(acc)
	}
	wg.Wait()
}

func (m *MaintenanceService) tick(ctx context.Context) {
	m.resumeAcceptedImages(ctx)
	m.reconcileOpenQuotaReservations(ctx)
	// 1. Re-activate quota-exhausted tokens whose reset time has passed, then
	//    auto-sync their real balance where the provider exposes a cheap probe.
	if recovered, err := m.tokens.RecoverQuota(ctx); err != nil {
		log.Printf("maintenance: recover_quota failed")
	} else if len(recovered) > 0 {
		log.Printf("maintenance: recovered %d quota token(s)", len(recovered))
		if m.tokenSvc != nil {
			go m.syncRecoveredQuota(recovered)
		}
	}

	// 1a. Roll Adobe's active reset marker forward so the admin view never shows
	//     a stale past time. Limited accounts remain owned by RecoverQuota.
	if _, err := m.tokens.RollResetMarkers(ctx, []string{"adobe"}); err != nil {
		log.Printf("maintenance: roll_reset failed")
	}

	// 1b. Runway tokens have no refresh — once the JWT expiry marker passes the
	//     token can only 401, so flip it to disabled+dead proactively instead of
	//     leaving a doomed account "active". Grok is intentionally NOT swept here:
	//     its reset marker is billingPeriodEnd (a credits-renewal date), not a
	//     death deadline — a grok sso keeps working past billingPeriodEnd, so
	//     expiring on it kills live accounts. Grok death is caught for real by the
	//     import-time FetchSession check and by marking dead on a 401 at use.
	for _, pool := range []string{"runway"} {
		if n, err := m.tokens.ExpireByReset(ctx, pool); err != nil {
			log.Printf("maintenance: expire_%s failed", pool)
		} else if n > 0 {
			log.Printf("maintenance: expired %d %s token(s)", n, pool)
		}
	}

	if m.tokenSvc != nil {
		// 1c. Re-sync Grok accounts through the authenticated credits endpoint.
		//     It also provides liveness; subscription tier is not a video
		//     entitlement signal and is intentionally not probed here.
		m.tokenSvc.RefreshGrokLiveness(ctx)
		// 1d. Refresh Oreate quota rows and active rows cached below the
		//     operating floor. Replenished accounts return to active automatically.
		m.tokenSvc.RefreshLowCreditOreateAccounts(ctx)
		// 1e. Re-probe ChatGPT accounts stuck in pending past stalePending — an
		//     import probe interrupted by a restart (or that never finished) would
		//     otherwise strand a good freshly-registered account forever, since
		//     RecoverQuota only revives 限额, never pending.
		m.tokenSvc.ReprobeStalePendingChatGPT(ctx, m.stalePending)
		// 1f. Same stale-pending safety net for Adobe: a cookie→token exchange
		//     interrupted mid-flight (process restart / redis blip during import)
		//     would otherwise strand the row as an empty-email pending zombie.
		m.tokenSvc.ReprobeStalePendingAdobe(ctx, m.stalePending)
	}

	// 2. Auto-renew Adobe cookies whose refresh interval has elapsed.
	if m.refresh != nil {
		if n, err := m.refresh.RefreshDue(ctx); err != nil {
			log.Printf("maintenance: refresh_due failed")
		} else if n > 0 {
			log.Printf("maintenance: refreshed %d cookie profile(s)", n)
		}
	}

	// 3. Fail long-pending events so credential tasks do not remain stuck after a
	//    process restart. 2API has no downstream credit debit/refund path.
	//    An event whose generation goroutine is still registered in-flight is
	//    NOT an orphan — a slow video render can legitimately outlive the
	//    window — so it is skipped and left to its own work-context backstop.
	//    Past the hard cap a still-registered entry means a wedged goroutine:
	//    purge + cancel as before.
	skipLive := func(e repo.StaleEvent) bool {
		return m.inflight != nil && m.inflight.Active(e.ID) && time.Since(e.TS) < 3*m.stalePending
	}
	if purged, err := m.events.PurgeStale(ctx, m.stalePending, skipLive); err != nil {
		log.Printf("maintenance: purge_stale failed")
	} else if len(purged) > 0 {
		cancelled := 0
		for _, e := range purged {
			// Stop the generation goroutine if it's still running, so it doesn't
			// keep grinding for minutes and surface a late "success" on this
			// just-abandoned event.
			if m.inflight != nil && m.inflight.Cancel(e.ID) {
				cancelled++
			}
			// Attribute the abandoned failure back to the account it was using
			// (the normal markTokenFailure path never ran for an orphaned job).
			if e.AccountID != "" {
				if err := m.tokens.IncrementFail(ctx, e.AccountID); err != nil {
					log.Printf("maintenance: fail-count abandoned event %s (account %s) failed", e.ID, e.AccountID)
				}
			}
		}
		log.Printf("maintenance: marked %d stale pending event(s) failed, cancelled %d in-flight", len(purged), cancelled)
	}

	// 4. Enforce the admin-configured log retention window.
	m.pruneLogs(ctx)

	// 5. Enforce the media retention window. Runs every 60s like the log prune;
	//    mediaPruneEvery still gates it in case the interval is ever shortened.
	if time.Since(m.lastMediaPrune) >= m.mediaPruneEvery {
		m.pruneMedia(ctx)
		m.lastMediaPrune = time.Now()
	}
}

// resumeAcceptedImages proactively restarts only the polling/download half of
// durable BytePlus tasks. It runs immediately after process start and every
// maintenance tick, so recovery does not depend on a downstream client polling.
// Ambiguous submissions have no upstream task id and are intentionally skipped.
func (m *MaintenanceService) resumeAcceptedImages(ctx context.Context) {
	if m.recovery == nil || m.models == nil || m.events == nil {
		return
	}
	attempts, err := m.models.Dispatch().ListAccepted(ctx, 100)
	if err != nil {
		log.Printf("maintenance: list accepted image tasks failed")
		return
	}
	for _, attempt := range attempts {
		event, eventErr := m.events.GetByID(ctx, attempt.EventID)
		if eventErr != nil {
			log.Printf("maintenance: load accepted image event failed")
			continue
		}
		m.recovery.startAcceptedImageRecovery(ctx, event)
	}
}

func (m *MaintenanceService) reconcileOpenQuotaReservations(ctx context.Context) {
	if m.models == nil || m.tokenSvc == nil {
		return
	}
	items, err := m.models.Quotas().ListOpen(ctx, time.Now().Add(-m.quotaRecoverAge), 200)
	if err != nil {
		log.Printf("maintenance: list open quota reservations failed")
		return
	}
	if len(items) == 0 {
		return
	}
	accounts, err := m.tokens.List(ctx)
	if err != nil {
		log.Printf("maintenance: list accounts for quota reconciliation failed")
		return
	}
	byID := make(map[string]model.TokenAccount, len(accounts))
	for _, account := range accounts {
		byID[account.ID] = account
	}
	for _, item := range items {
		state, failureClass := "unknown", ""
		if item.Attempt != nil {
			state, failureClass = item.Attempt.State, item.Attempt.FailureClass
		}
		reservationID := item.Reservation.ID
		switch state {
		case "failed":
			if failureClass == "quota" {
				zero := 0.0
				if err := m.models.Quotas().Settle(ctx, reservationID, &zero); err != nil {
					log.Printf("maintenance: settle exhausted reservation %s failed", reservationID)
				}
			} else if err := m.models.Quotas().Release(ctx, reservationID); err != nil {
				log.Printf("maintenance: release reservation %s failed", reservationID)
			}
			continue
		case "unknown", "submitting", "created":
			if err := m.models.Quotas().MarkUncertain(ctx, reservationID); err != nil {
				log.Printf("maintenance: mark reservation %s uncertain failed", reservationID)
				continue
			}
		}
		account, ok := byID[item.Bucket.AccountID]
		if !ok {
			// Missing credentials make the upstream outcome unknowable. Keep the
			// reservation conservative instead of inventing spendable quota.
			continue
		}
		remaining := m.refreshRecoveredQuota(ctx, account, item.Bucket.BucketKey)
		if remaining == nil && (state == "unknown" || state == "submitting" || state == "created") {
			continue
		}
		if err := m.models.Quotas().Settle(ctx, reservationID, remaining); err != nil {
			log.Printf("maintenance: settle reservation %s failed", reservationID)
		}
	}
}

func (m *MaintenanceService) refreshRecoveredQuota(ctx context.Context, account model.TokenAccount, bucketKey string) *float64 {
	authoritativeBucket, unit, scoped := providerSnapshotScope(account.Pool)
	if !scoped || strings.TrimSpace(bucketKey) != authoritativeBucket {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	snapshot, err := m.tokenSvc.Quota(probeCtx, account.Pool, account.ID)
	if err != nil || boolValueWithDefault(snapshot["unknown"], false) {
		return nil
	}
	remaining, ok := anyFloat(snapshot["remaining"])
	if !ok {
		return nil
	}
	var total *float64
	if value, exists := anyFloat(snapshot["total"]); exists {
		total = &value
	}
	if _, err := m.models.Quotas().UpsertSnapshot(ctx, account.ID, bucketKey, unit, total, &remaining, parseResetTime(snapshot["reset_after"])); err != nil {
		log.Printf("maintenance: refresh recovered quota snapshot failed")
		return nil
	}
	return &remaining
}

func (m *MaintenanceService) pruneLogs(ctx context.Context) {
	days := m.retentionDays(ctx, "logs.retention_days")
	if days <= 0 {
		return
	}
	if _, err := m.events.PurgeOlderThan(ctx, time.Duration(days)*24*time.Hour); err != nil {
		log.Printf("maintenance: purge_older_than failed")
	}
}

func (m *MaintenanceService) pruneMedia(ctx context.Context) {
	if m.store == nil || !m.store.Configured() {
		return
	}
	days := m.retentionDays(ctx, "artifacts.retention_days")
	if days <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	objs, err := m.store.List(ctx, "")
	if err != nil {
		log.Printf("maintenance: list media failed")
		return
	}
	removed := 0
	var clearedKeys []string
	for _, o := range objs {
		if !o.LastModified.Before(cutoff) {
			continue
		}
		if err := m.store.Delete(ctx, o.Key); err != nil {
			log.Printf("maintenance: delete %s failed", o.Key)
			continue
		}
		removed++
		// event_log.file stores the same key — blank those rows so the log views
		// don't dangle a 404 preview.
		clearedKeys = append(clearedKeys, o.Key)
	}
	if removed > 0 {
		log.Printf("maintenance: pruned %d expired artifact object(s)", removed)
	}
	if len(clearedKeys) > 0 {
		if n, err := m.events.ClearFiles(ctx, clearedKeys); err != nil {
			log.Printf("maintenance: clear_files failed")
		} else if n > 0 {
			log.Printf("maintenance: cleared file ref on %d log row(s)", n)
		}
	}
}

func (m *MaintenanceService) retentionDays(ctx context.Context, key string) int {
	raw, err := m.settings.GetValue(ctx, key)
	if err != nil {
		return 0
	}
	days, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || days <= 0 {
		return 0
	}
	return days
}
