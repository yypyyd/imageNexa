package service

import (
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"backend/internal/model"
	"backend/internal/provider/byteplus"
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
	quotaRefreshing atomic.Bool
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

// syncRecoveredQuota refreshes both display metadata and the authoritative
// scheduling bucket after recovery. Bounded concurrency absorbs reset bursts.
func (m *MaintenanceService) syncRecoveredQuota(parent context.Context, accs []model.TokenAccount) {
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, acc := range accs {
		if _, _, scoped := providerSnapshotScope(acc.Pool); !scoped {
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-parent.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(a model.TokenAccount) {
			defer wg.Done()
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(parent, 45*time.Second)
			defer cancel()
			_, _ = m.tokenSvc.Quota(ctx, a.Pool, a.ID)
		}(acc)
	}
	wg.Wait()
}

// refreshDueQuota repairs zero/old buckets even when no request can select the
// account. The database claim also throttles failed probes across processes.
func (m *MaintenanceService) refreshDueQuota(ctx context.Context) {
	if m.models == nil || m.tokenSvc == nil || !m.quotaRefreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer m.quotaRefreshing.Store(false)
		buckets, err := m.models.Quotas().ClaimRefreshDue(ctx, time.Now(), 20)
		if err != nil {
			recordBookkeepingError("claim quota refresh", err)
			return
		}
		accounts := make([]model.TokenAccount, 0, len(buckets))
		seen := make(map[string]bool, len(buckets))
		for _, bucket := range buckets {
			if seen[bucket.AccountID] {
				continue
			}
			// The scoped probe is account-wide; never probe an obsolete text bucket
			// with an image/media endpoint.
			for _, pool := range SchedulableProviders() {
				key, _, ok := providerSnapshotScope(pool)
				if ok && key == bucket.BucketKey {
					seen[bucket.AccountID] = true
					accounts = append(accounts, model.TokenAccount{ID: bucket.AccountID, Pool: pool})
					break
				}
			}
		}
		m.syncRecoveredQuota(ctx, accounts)
	}()
}

func (m *MaintenanceService) tick(ctx context.Context) {
	if m.tokenSvc != nil {
		m.tokenSvc.ReprobeDolaReadiness(ctx)
	}
	m.resumeAcceptedImages(ctx)
	// Verified-absent submissions flip to failed here so the reservation sweep
	// below releases their hold in the same tick.
	m.reconcileUnknownSubmissions(ctx)
	m.reconcileOpenQuotaReservations(ctx)
	m.refreshDueQuota(ctx)
	// 1. Re-activate quota-exhausted tokens whose reset time has passed, then
	//    auto-sync their real balance where the provider exposes a cheap probe.
	if recovered, err := m.tokens.RecoverQuota(ctx); err != nil {
		log.Printf("maintenance: recover_quota failed")
	} else if len(recovered) > 0 {
		log.Printf("maintenance: recovered %d quota token(s)", len(recovered))
		if m.tokenSvc != nil {
			go m.syncRecoveredQuota(ctx, recovered)
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
		if n, err := m.tokenSvc.ExpireChatGPTTokens(ctx); err != nil {
			log.Printf("maintenance: expire_chatgpt failed")
		} else if n > 0 {
			log.Printf("maintenance: expired %d chatgpt token(s)", n)
		}
		// 1c. Re-sync Grok accounts through the authenticated credits endpoint.
		//     It also provides liveness; subscription tier is not a video
		//     entitlement signal and is intentionally not probed here.
		m.tokenSvc.RefreshGrokLiveness(ctx)
		// 1d. Refresh Oreate quota rows and active rows cached below the
		//     operating floor. Replenished accounts return to active automatically.
		m.tokenSvc.RefreshLowCreditOreateAccounts(ctx)
		// 1e. Keep Oreate website sessions warm without submitting paid work.
		m.tokenSvc.RefreshOreateSessions(ctx)
		// 1f. Re-probe ChatGPT accounts stuck in pending past stalePending — an
		//     import probe interrupted by a restart (or that never finished) would
		//     otherwise strand a good freshly-registered account forever, since
		//     RecoverQuota only revives 限额, never pending.
		m.tokenSvc.ReprobeStalePendingChatGPT(ctx, m.stalePending)
		// 1g. Same stale-pending safety net for Adobe: a cookie→token exchange
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
		if strings.HasPrefix(attempt.ModelRouteID, "video.") {
			continue
		} // Image recovery cannot prove a video was uncharged.
		event, eventErr := m.events.GetByID(ctx, attempt.EventID)
		if eventErr != nil {
			log.Printf("maintenance: load accepted image event failed")
			continue
		}
		m.recovery.startAcceptedImageRecovery(ctx, event)
	}
}

// Ambiguous create_task outcomes are verified against the provider's own task
// history only after the create timeout has fully elapsed, so a slowly accepted
// task cannot be missed. Past the hard cap an event whose account can no longer
// be read (dead cookie, persistent upstream failure) is closed anyway: BytePlus
// finishes image tasks within minutes, and pinning a downstream client on
// "in_progress" forever is worse than a rare lost image.
const (
	unknownSubmissionVerifyAfter = 2 * time.Minute
	unknownSubmissionHardCap     = time.Hour
	unknownSubmissionSlack       = time.Minute
)

// reconcileUnknownSubmissions closes the loop on BytePlus create_task calls whose
// response was lost. A task found in the account's history is adopted and then
// resumed like any accepted task; a verified absence fails the event without
// ever replaying the non-idempotent submission.
func (m *MaintenanceService) reconcileUnknownSubmissions(ctx context.Context) {
	if m.recovery == nil || m.models == nil || m.events == nil || m.tokens == nil {
		return
	}
	attempts, err := m.models.Dispatch().ListUnknown(ctx, time.Now().Add(-unknownSubmissionVerifyAfter), 100)
	if err != nil {
		log.Printf("maintenance: list unknown submissions failed")
		return
	}
	if len(attempts) == 0 {
		return
	}
	m.recovery.applyGlobalProxy(ctx)
	adopted, closed := 0, 0
	for _, attempt := range attempts {
		if strings.HasPrefix(attempt.ModelRouteID, "video.") {
			continue
		} // Image recovery cannot prove a video was uncharged.
		event, eventErr := m.events.GetByID(ctx, attempt.EventID)
		if eventErr != nil {
			log.Printf("maintenance: load unknown submission event failed")
			continue
		}
		if event == nil || event.Status != "pending" {
			// The event was closed by another path; the attempt row is historical.
			// Mark it so the reservation sweep can release the hold.
			m.finishUnknownSubmission(ctx, attempt, event)
			continue
		}
		verdict, taskID := m.verifyUnknownSubmission(ctx, attempt, event)
		switch decideUnknownSubmission(verdict, attempt.StartedAt, time.Now()) {
		case adoptUnknownSubmission:
			if err := m.models.Dispatch().Adopt(ctx, attempt.ID, taskID); err != nil {
				log.Printf("maintenance: adopt verified submission failed")
				continue
			}
			m.recovery.startAcceptedImageRecovery(ctx, event)
			adopted++
		case closeUnknownSubmission:
			m.finishUnknownSubmission(ctx, attempt, event)
			closed++
		}
	}
	if adopted > 0 || closed > 0 {
		log.Printf("maintenance: reconciled unknown submissions: adopted %d, closed %d", adopted, closed)
	}
}

type unknownSubmissionVerdict int

const (
	unknownSubmissionUnverified unknownSubmissionVerdict = iota
	unknownSubmissionAccepted
	unknownSubmissionAbsent
)

type unknownSubmissionAction int

const (
	keepUnknownSubmission unknownSubmissionAction = iota
	adoptUnknownSubmission
	closeUnknownSubmission
)

// decideUnknownSubmission turns a history verdict into a state transition. A
// provider verdict always wins; only the hard cap may close an attempt whose
// history could not be read.
func decideUnknownSubmission(verdict unknownSubmissionVerdict, startedAt, now time.Time) unknownSubmissionAction {
	switch verdict {
	case unknownSubmissionAccepted:
		return adoptUnknownSubmission
	case unknownSubmissionAbsent:
		return closeUnknownSubmission
	}
	if now.Sub(startedAt) > unknownSubmissionHardCap {
		return closeUnknownSubmission
	}
	return keepUnknownSubmission
}

// verifyUnknownSubmission consults the account's task history. The returned task
// id is only meaningful for unknownSubmissionAccepted.
func (m *MaintenanceService) verifyUnknownSubmission(ctx context.Context, attempt model.DispatchAttempt, event *model.EventLog) (unknownSubmissionVerdict, string) {
	if event.Provider != "byteplus" || m.recovery.byteplus == nil {
		// Only BytePlus produces ambiguous submissions today; anything else has no
		// history to consult and can only age out.
		return unknownSubmissionUnverified, ""
	}
	account, err := m.tokens.Get(ctx, "byteplus", attempt.AccountID)
	if err != nil || account == nil || strings.TrimSpace(account.Value) == "" {
		return unknownSubmissionUnverified, ""
	}
	providerModel := event.Model
	if route, routeErr := m.models.Routes().GetRoute(ctx, attempt.ModelRouteID); routeErr == nil && route != nil {
		if upstream := strings.TrimSpace(route.UpstreamModel); upstream != "" {
			providerModel = upstream
		}
	}
	finished := attempt.StartedAt
	if attempt.FinishedAt != nil {
		finished = *attempt.FinishedAt
	}
	probeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	taskID, err := m.recovery.byteplus.FindSubmittedTask(probeCtx, account.Value, byteplus.SubmittedTaskQuery{
		Model:  providerModel,
		Prompt: event.Prompt,
		From:   attempt.StartedAt.Add(-unknownSubmissionSlack),
		To:     finished.Add(unknownSubmissionSlack),
	})
	if err != nil {
		return unknownSubmissionUnverified, ""
	}
	if taskID == "" {
		return unknownSubmissionAbsent, ""
	}
	return unknownSubmissionAccepted, taskID
}

// finishUnknownSubmission records the terminal verdict. The attempt becomes a
// plain temporary failure (the reservation sweep releases its hold), the event
// leaves pending, and the account/route binding learns about the failure.
func (m *MaintenanceService) finishUnknownSubmission(ctx context.Context, attempt model.DispatchAttempt, event *model.EventLog) {
	if err := m.models.Dispatch().Finish(ctx, attempt.ID, "failed", "temporary", "", ErrProviderTemporary); err != nil {
		log.Printf("maintenance: close unknown submission failed")
		return
	}
	if event != nil && event.Status == "pending" {
		if err := m.events.UpdateStatus(ctx, event.ID, "failed", ErrProviderTemporary.Error(), 0); err != nil {
			log.Printf("maintenance: fail unverified pending event failed")
		}
		if m.inflight != nil {
			m.inflight.Cancel(event.ID)
		}
	}
	if attempt.AccountID != "" && attempt.ModelRouteID != "" {
		if err := m.models.Routes().RecordAccountRouteResult(ctx, attempt.AccountID, attempt.ModelRouteID, "temporary", false); err != nil {
			log.Printf("maintenance: record unknown submission route result failed")
		}
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
	accountIDs := make([]string, 0, len(items))
	seenAccount := make(map[string]struct{}, len(items))
	for _, item := range items {
		accountID := strings.TrimSpace(item.Bucket.AccountID)
		if accountID == "" {
			continue
		}
		if _, exists := seenAccount[accountID]; exists {
			continue
		}
		seenAccount[accountID] = struct{}{}
		accountIDs = append(accountIDs, accountID)
	}
	accounts, err := m.tokens.ListIdentityByIDs(ctx, accountIDs)
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
	authoritativeBucket, _, scoped := providerSnapshotScope(account.Pool)
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
