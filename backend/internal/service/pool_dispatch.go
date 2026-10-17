package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"backend/internal/model"
	"backend/internal/provider/adobe"
	"backend/internal/provider/byteplus"
	"backend/internal/provider/chatgpt"
	"backend/internal/provider/dola"
	"backend/internal/provider/grok"
	"backend/internal/repo"

	"gorm.io/gorm"
)

type verifiedBytePlusRetryFailure struct {
	cause    error
	baseline float64
}

func (failure *verifiedBytePlusRetryFailure) Error() string { return failure.cause.Error() }

func (failure *verifiedBytePlusRetryFailure) Unwrap() []error {
	return []error{failure.cause, errBytePlusRetryVerified}
}

func verifiedBytePlusRetry(cause error, baseline float64) error {
	return &verifiedBytePlusRetryFailure{cause: cause, baseline: baseline}
}

func bytePlusRetryVerification(err error) (*verifiedBytePlusRetryFailure, bool) {
	var failure *verifiedBytePlusRetryFailure
	if !errors.As(err, &failure) {
		return nil, false
	}
	return failure, true
}

func finalizeBytePlusRetryVerification(err error, remaining *float64) error {
	verification, ok := bytePlusRetryVerification(err)
	if !ok {
		return err
	}
	if remaining == nil || math.Abs(*remaining-verification.baseline) > 1e-9 {
		return verification.cause
	}
	return err
}

// grokConcurrencyPerAccount is how many simultaneous generations one grok account
// may run (grok tolerates 10, unlike the 1-per-account default elsewhere).
const grokConcurrencyPerAccount = 10

// Adobe partner/points accounts use a bounded four-way simultaneous-generation
// limit. This is also the default for newly imported accounts when no override
// is stored, while keeping burst traffic below the provider's risk threshold.
const adobePointsConcurrencyPerAccount = 4

// Absorb short bursts above the account pool capacity instead of rejecting them
// immediately. The generation context owns the upper bound for the whole job.
// accountFailureCooldown is how long an account stays demoted after an upstream
// (auth/temporary/fatal) failure. Short on purpose: it only reorders the
// rotation, and a pool where every account is cooling behaves exactly as before.
const accountFailureCooldown = 45 * time.Second

const providerAccountQueueWait = 90 * time.Second

const providerAccountQueuePoll = 300 * time.Millisecond

// A real 10-way production burst on 2026-09-05 saw seven first-account beta
// failures and three requests whose second account hit the same verdict. Six
// distinct accounts keeps the retry bounded while making a repeated beta blip
// very unlikely to escape to the caller. Every failed task must independently
// pass the four-snapshot no-charge proof before it consumes another slot in
// this budget.
const maxBytePlusBetaAccountAttempts = 6

type bytePlusBetaRetryBudget struct {
	active              bool
	attempts            int
	lastErr             error
	nextAccountDeadline time.Time
}

// handle consumes one completed account result after the special retry chain
// has started (or starts it for the first verified beta failure). retry=true
// means the caller may select another, not-yet-attempted BytePlus account.
// A non-beta result ends the chain: accepted/ambiguous work keeps its native
// no-resubmit error, while a definite pre-acceptance failure receives the
// private exhausted marker solely to prevent cross-route fan-out.
func (budget *bytePlusBetaRetryBudget) handle(err error, now time.Time) (retry bool, terminal error) {
	verified := bytePlusBetaRetryVerified(err)
	if !budget.active && !verified {
		return false, err
	}
	if !verified {
		if byteplusNoResubmit(err) {
			return false, err
		}
		return false, errors.Join(err, errBytePlusRetryBudgetExhausted)
	}

	budget.active = true
	budget.attempts++
	budget.lastErr = err
	// Provider execution can itself take longer than the ordinary queue window.
	// Start a fresh bounded wait after each verified terminal result so a free
	// alternate is not rejected merely because the previous task ran slowly.
	budget.nextAccountDeadline = now.Add(providerAccountQueueWait)
	if budget.attempts >= maxBytePlusBetaAccountAttempts {
		return false, errors.Join(err, errBytePlusRetryBudgetExhausted)
	}
	return true, nil
}

// runPoolWithFailover drives a generation across a round-robin-ordered account
// list with per-error-class behavior, so a bad request never burns the whole
// pool while genuinely limited accounts still fail over:
//   - 额度耗尽 quota → mark the account and FAIL OVER to the next account
//     immediately (same-account retry can't help). Repeats until one succeeds or
//     the pool is exhausted.
//   - 认证失效 auth → refresh the token from its cookie and retry ONCE with the
//     fresh token; if it still auth-fails (or there's nothing to refresh, e.g.
//     chatgpt's JWT IS the credential), mark the account and fail over.
//   - 上游临时 temporary → record the failure (no disable/dead) and FAIL OVER to
//     the next unused account immediately. The failed credential is excluded
//     for the rest of the request. Identical temporary signatures stop after
//     three accounts; mixed failures may use up to six distinct accounts so a
//     few bad credentials cannot idle the rest of the pool.
//   - 参数错 / request-level (anything else) → return immediately, no retry, no
//     account penalty (the account isn't at fault).
//
// Returns the actual upstream error (never a synthetic "retry failed"). On
// success it stamps success_total/fails=0 on the winning account. classify maps
// a provider error to (isAuth, isQuota, isTemporary, isDead). refreshOnAuth
// (nil for providers whose token IS the credential) re-mints the account's token
// so an auth retry uses a FRESH token instead of replaying the stale one.
func (s *V1Service) runPoolWithFailover(ctx context.Context, eventID, pool string, active []model.TokenAccount, kind string,
	attempt func(token model.TokenAccount) ([]byte, error),
	classify func(error) (isAuth, isQuota, isTemporary, isDead bool),
	refreshOnAuth func(tokenID string) (model.TokenAccount, bool),
	tempFailover bool,
) ([]byte, error) {
	return s.runPoolWithFailoverPolicy(ctx, eventID, pool, active, kind, attempt, classify, refreshOnAuth, tempFailover, true)
}

// runPoolWithFailoverPolicy is the policy-bearing implementation. Temporary
// failures exclude that credential and continue to unused accounts. Adobe
// third-party images still pass retryTemporary=false so a later cross-provider
// fallback is not delayed; the flag is retained for call-site compatibility.
func (s *V1Service) runPoolWithFailoverPolicy(ctx context.Context, eventID, pool string, active []model.TokenAccount, kind string,
	attempt func(token model.TokenAccount) ([]byte, error),
	classify func(error) (isAuth, isQuota, isTemporary, isDead bool),
	refreshOnAuth func(tokenID string) (model.TokenAccount, bool),
	tempFailover bool,
	retryTemporary bool,
) ([]byte, error) {
	if plan, ok := dispatchPlanFromContext(ctx); ok {
		var routeErr error
		active, routeErr = s.routeAccounts(ctx, plan.Route, active)
		if routeErr != nil {
			return nil, routeErr
		}
		if len(active) == 0 {
			return nil, ErrNoProviderAccount
		}
	}
	policy := poolPolicy(ctx)
	_ = retryTemporary
	requestState := policy.accountRequest(ctx, pool)
	excludedAccounts := requestState.excluded
	var refreshedAt time.Time
	var tempState tempFailoverState
	var durableErr error
	queueDeadline := time.Now().Add(providerAccountQueueWait)
	// A verified terminal BytePlus beta-instability verdict may walk a bounded
	// set of different accounts. The attempted set prevents cycling back to any
	// account already used by this request; the special budget also bypasses the
	// ordinary temporary-account budget and never crosses provider routes.
	var bytePlusBetaRetry bytePlusBetaRetryBudget
	attemptedAccounts := requestState.attempted
	for {
		var refreshErr error
		if time.Since(refreshedAt) >= time.Second {
			active, refreshErr = s.refreshPoolAccounts(ctx, pool, kind, active, excludedAccounts)
			s.demoteCoolingAccounts(pool, active)
			refreshedAt = time.Now()
		}
		if refreshErr != nil || len(active) == 0 {
			if bytePlusBetaRetry.active {
				return nil, errors.Join(bytePlusBetaRetry.lastErr, refreshErr)
			}
			if refreshErr != nil {
				return nil, refreshErr
			}
			if durableErr != nil {
				return nil, durableErr
			}
			return nil, ErrNoProviderAccount
		}
		counts, observed := s.conc.ActiveCounts(ctx, accountGateKeys(active))
		var lastErr error
		busy := 0
		for _, token := range active {
			if excludedAccounts[token.ID] {
				continue
			}
			if bytePlusBetaRetry.active {
				if _, alreadyTried := attemptedAccounts[token.ID]; alreadyTried {
					continue
				}
			}
			slots := poolAccountConcurrency(pool, token)
			if observed && counts["conc:a:"+token.ID] >= int64(slots) {
				busy++
				continue
			}
			admitted, gateErr := s.acctAcquire(ctx, token.ID, eventID, slots)
			if gateErr != nil {
				if bytePlusBetaRetry.active {
					return nil, errors.Join(bytePlusBetaRetry.lastErr, gateErr)
				}
				return nil, gateErr
			}
			if !admitted {
				busy++
				continue
			}
			_, retry := attemptedAccounts[token.ID]
			attemptedAccounts[token.ID] = struct{}{}
			// release via defer so a panic in tryAccount can't leak the job slot.
			data, failover, tempDead, err := func() ([]byte, bool, bool, error) {
				defer s.acctRelease(ctx, token.ID, eventID)
				return s.tryAccount(ctx, eventID, pool, token, kind, attempt, classify, refreshOnAuth, tempFailover, retry)
			}()
			if err == nil {
				return data, nil
			}
			lastErr = err
			durableErr = err
			isAuth, isQuota, _, isDead := classify(err)
			if excludeAfterAccountFailure(isAuth, isQuota, isDead, tempDead, err) {
				excludedAccounts[token.ID] = true
			}
			// Once the job's deadline is spent, another account can only fail on
			// the expired context and would mask the failure that consumed it.
			if ctx.Err() != nil {
				if bytePlusBetaRetry.active {
					// Preserve the verified-beta marker even when an alternate's final
					// bookkeeping races the generation deadline. Returning only the
					// alternate error here could incorrectly authorize route failover.
					return nil, errors.Join(bytePlusBetaRetry.lastErr, lastErr, ctx.Err())
				}
				return nil, lastErr
			}
			if bytePlusBetaRetry.active || (pool == "byteplus" && bytePlusBetaRetryVerified(err)) {
				retry, terminal := bytePlusBetaRetry.handle(err, time.Now())
				if terminal != nil {
					return nil, terminal
				}
				if retry {
					continue
				}
			}
			if tempDead {
				// Unused accounts are tried immediately. The same credential is
				// never retried in this request; identical signatures stop the
				// wave so a shared outage cannot walk the pool.
				if tempState.note(err) {
					return nil, lastErr
				}
			}
			if failover {
				continue
			}
			// temporary exhausted or request-level error → surface it, no fan-out.
			return nil, lastErr
		}

		if bytePlusBetaRetry.active && busy == 0 {
			return nil, bytePlusBetaRetry.lastErr
		}
		if lastErr != nil && busy == 0 && !bytePlusBetaRetry.active {
			return nil, lastErr
		}
		if busy == 0 {
			return nil, ErrProviderExecution
		}
		if policy.fastFailover && !bytePlusBetaRetry.active {
			return nil, ErrConcurrencyFull
		}
		accountQueueDeadline := queueDeadline
		if bytePlusBetaRetry.active {
			accountQueueDeadline = bytePlusBetaRetry.nextAccountDeadline
		}
		if time.Now().After(accountQueueDeadline) {
			if bytePlusBetaRetry.active {
				return nil, bytePlusBetaRetry.lastErr
			}
			return nil, ErrConcurrencyFull
		}

		if !waitAccountQueue(ctx, accountQueueDeadline) {
			if bytePlusBetaRetry.active {
				return nil, errors.Join(bytePlusBetaRetry.lastErr, ctx.Err())
			}
			return nil, ErrConcurrencyFull
		}
	}
}

// tryAccount runs one account's attempt with the pool's retry policy:
// 额度耗尽/认证失效 → mark + failover; 上游临时 → record failure + failover (capped
// via the tempDead return); 参数错 → fail fast. Returns (data, failover,
// tempDead, err) — failover=true means move on to the next account. The per-account
// concurrency gate is held by the caller.
func (s *V1Service) tryAccount(ctx context.Context, eventID, pool string, token model.TokenAccount, kind string,
	attempt func(token model.TokenAccount) ([]byte, error),
	classify func(error) (isAuth, isQuota, isTemporary, isDead bool),
	refreshOnAuth func(tokenID string) (model.TokenAccount, bool),
	tempFailover bool,
	retry bool,
) ([]byte, bool, bool, error) {
	routeID := dispatchRouteFromContext(ctx)
	if plan, ok := dispatchPlanFromContext(ctx); ok && pool == "dola" {
		plan.QuotaDay = time.Now()
		ctx = context.WithValue(ctx, dispatchRouteKey, plan)
	}
	fresh, validationErr := s.revalidateDispatchAccount(ctx, pool, token.ID, kind, retry)
	if validationErr != nil {
		return nil, errors.Is(validationErr, ErrNoProviderAccount) || routeFailoverSafe(validationErr), false, validationErr
	}
	token = fresh
	recordBookkeepingError("set generation account", s.events.SetAccount(ctx, eventID, token.ID, token.AccountEmail))
	recordBookkeepingError("touch generation account", s.tokens.TouchLastUsed(ctx, token.ID))
	authRefreshed := false
	for {
		if authRefreshed {
			fresh, err := s.revalidateDispatchAccount(ctx, pool, token.ID, kind, true)
			if err != nil {
				return nil, errors.Is(err, ErrNoProviderAccount) || routeFailoverSafe(err), false, err
			}
			token = fresh
		}
		var dispatchID string
		var reservation *model.QuotaReservation
		if routeID != "" {
			attemptRow, startErr := s.models.Dispatch().Start(context.WithoutCancel(ctx), eventID, routeID, token.ID)
			if startErr != nil {
				return nil, false, false, startErr
			}
			dispatchID = attemptRow.ID
			plan, _ := dispatchPlanFromContext(ctx)
			bucketKey := strings.TrimSpace(plan.Route.QuotaBucketKey)
			if binding, bindingErr := s.models.Routes().AccountRoute(ctx, token.ID, routeID); bindingErr == nil && strings.TrimSpace(binding.QuotaBucketKey) != "" {
				bucketKey = strings.TrimSpace(binding.QuotaBucketKey)
			}
			bucketKey = dispatchQuotaBucket(plan, bucketKey)
			if bucketKey != "" && plan.Cost > 0 {
				unit := "credits"
				if policy, ok := model.DecodeQuotaCostPolicy(plan.Route.QuotaCosts); ok && strings.TrimSpace(policy.Unit) != "" {
					unit = strings.TrimSpace(policy.Unit)
				}
				reservation, startErr = s.models.Quotas().ReserveWithUnit(context.WithoutCancel(ctx), eventID, dispatchID, token.ID, bucketKey, unit, plan.Cost)
				if startErr != nil {
					if !errors.Is(startErr, repo.ErrQuotaUnavailable) {
						recordBookkeepingError("finish failed quota reservation", s.models.Dispatch().Finish(context.WithoutCancel(ctx), dispatchID, "failed", "temporary", "", errors.New("quota reservation unavailable")))
						return nil, false, false, startErr
					}
					quotaErr := providerQuotaError(pool, startErr)
					recordBookkeepingError("finish rejected quota dispatch", s.models.Dispatch().Finish(context.WithoutCancel(ctx), dispatchID, "failed", "quota", "", publicGenerationError(quotaErr)))
					return nil, true, false, quotaErr
				}
			}
		}
		recordBookkeepingError("mark generation submission phase", s.events.SetPhase(context.WithoutCancel(ctx), eventID, "submitting"))
		token.QuotaReserved = reservation != nil && reservation.BalanceKnown
		data, err := attempt(token)
		var upstreamRemaining *float64
		plan, _ := dispatchPlanFromContext(ctx)
		if routeID != "" && shouldRefreshDispatchQuota(plan, pool, reservation, err) {
			probeCtx := ctx
			if bytePlusBetaRetryVerified(err) {
				probeCtx = context.WithValue(ctx, quotaVerificationKey{}, true)
			}
			upstreamRemaining = s.refreshDispatchQuota(probeCtx, pool, token, kind)
		}
		finalizedErr := finalizeBytePlusRetryVerification(err, upstreamRemaining)
		if finalizedErr != err {
			// The final quota refresh is newer than the closure's two confirmations.
			// A delayed debit or unavailable snapshot revokes retry permission while
			// preserving the provider's accepted terminal error and task ID.
			err = finalizedErr
		}
		state, failureClass := dispatchFailureClass(err)
		if reservation != nil {
			quotaCtx := context.WithoutCancel(ctx)
			if errors.Is(err, dola.ErrTaskAccepted) || errors.Is(err, chatgpt.ErrTaskAccepted) || errors.Is(err, grok.ErrTaskAccepted) {
				// A terminal content rejection still consumed an accepted attempt.
				// Closing its dispatch must not refund the daily generation.
				recordBookkeepingError("settle accepted Dola dispatch quota", s.models.Quotas().Settle(quotaCtx, reservation.ID, upstreamRemaining))
			} else if errors.Is(err, byteplus.ErrRetryableTaskFailed) && !bytePlusBetaRetryVerified(err) {
				// The task is definitely terminal, so close its dispatch; without a
				// no-charge proof, settle rather than refund the reservation.
				recordBookkeepingError("settle unverified beta dispatch quota", s.models.Quotas().Settle(quotaCtx, reservation.ID, upstreamRemaining))
			} else {
				switch state {
				case "succeeded", "accepted":
					recordBookkeepingError("settle dispatch quota", s.models.Quotas().Settle(quotaCtx, reservation.ID, upstreamRemaining))
				case "unknown":
					recordBookkeepingError("mark dispatch quota uncertain", s.models.Quotas().MarkUncertain(quotaCtx, reservation.ID))
				default:
					if failureClass == "quota" {
						// An explicit provider quota verdict is authoritative. Releasing the
						// hold after a refreshed zero would add the request cost back and make
						// the exhausted bucket immediately schedulable again.
						if upstreamRemaining == nil {
							zero := 0.0
							upstreamRemaining = &zero
						}
						recordBookkeepingError("settle exhausted dispatch quota", s.models.Quotas().Settle(quotaCtx, reservation.ID, upstreamRemaining))
					} else {
						recordBookkeepingError("release dispatch quota", s.models.Quotas().Release(quotaCtx, reservation.ID))
					}
				}
			}
		}
		if dispatchID != "" {
			recordBookkeepingError("finish dispatch attempt", s.models.Dispatch().Finish(context.WithoutCancel(ctx), dispatchID, state, failureClass, dispatchUpstreamTaskID(err), publicGenerationError(err)))
			recordBookkeepingError("record account route result", s.models.Routes().RecordAccountRouteResult(context.WithoutCancel(ctx), token.ID, routeID, failureClass, err == nil))
		}
		if err == nil {
			_, updateErr := s.tokens.Update(ctx, pool, token.ID, map[string]any{
				"last_used_at":  time.Now(),
				"success_total": gorm.Expr("success_total + 1"),
				"fails":         0,
			})
			recordBookkeepingError("record account success", updateErr)
			return data, false, false, nil
		}
		if failureClass == "entitlement" {
			// Entitlement is scoped to this account+route. The durable binding was
			// disabled above; the credential remains healthy for every other route.
			return nil, true, false, err
		}
		if errors.Is(err, grok.ErrTaskAccepted) && failureClass == "quota" {
			s.markTokenFailure(ctx, pool, token, kind, false, true)
		}
		if errors.Is(err, chatgpt.ErrTaskAccepted) || errors.Is(err, chatgpt.ErrTaskSubmissionUnknown) || errors.Is(err, grok.ErrTaskAccepted) {
			return nil, false, false, err
		}
		isAuth, isQuota, isTemp, isDead := classify(err)
		if isQuota {
			if shouldMarkAccountQuota(err) {
				s.markTokenFailure(ctx, pool, token, kind, false, true)
			}
			return nil, true, false, err
		}
		if isAuth {
			// Refresh from cookie and retry ONCE; otherwise the credential is dead.
			if refreshOnAuth != nil && !authRefreshed {
				if refreshed, ok := refreshOnAuth(token.ID); ok {
					token = refreshed
					authRefreshed = true
					continue
				}
			}
			s.markTokenFailure(ctx, pool, token, kind, true, false)
			s.coolDownAccountWithToken(pool, token)
			return nil, true, false, err
		}
		// Fatal / temporary-under-failover-policy upstream error.
		if isDead || (isTemp && tempFailover) {
			if tempFailover {
				// Ops policy (adobe): NEVER kill on these upstream errors — a
				// genuinely bad account and a transient Adobe blip (429/5xx/
				// overload) look the same, and killing wipes healthy accounts.
				// Record the failure and fail over to the next unused account (no
				// disable/dead). The caller excludes this credential and stops
				// after identical temporary signatures or the unique-account cap.
				s.markTokenUpstreamFailure(ctx, pool, token)
				s.coolDownAccountWithToken(pool, token)
				return nil, true, true, err
			}
			s.markTokenDead(ctx, pool, token, kind)
			// Permanent account death must not consume the temporary-failure cap.
			return nil, true, false, err
		}
		if isTemp {
			// Temporary upstream error → record it against the upstream (not the
			// account) and fail over to the NEXT account, capped via the tempDead
			// return so a pool-wide blip can't fan one request across the whole pool.
			s.markTokenUpstreamFailure(ctx, pool, token)
			s.coolDownAccountWithToken(pool, token)
			return nil, true, true, err
		}
		return nil, false, false, err // 参数错 / request-level
	}
}

func dispatchUpstreamTaskID(err error) string {
	if id := chatgpt.AcceptedTaskID(err); id != "" {
		return id
	}
	if taskURL := grok.AcceptedTaskURL(err); taskURL != "" {
		return taskURL
	}
	var dolaErr *dola.VideoSubmissionError
	if errors.As(err, &dolaErr) {
		return dolaErr.ConversationID
	}
	if err == nil {
		return ""
	}
	return byteplus.AcceptedTaskID(err)
}

func providerQuotaError(pool string, cause error) error {
	switch pool {
	case "adobe":
		return fmt.Errorf("%w: %w", adobe.ErrQuotaExhausted, cause)
	case "byteplus":
		return fmt.Errorf("%w: %w", byteplus.ErrQuotaExhausted, cause)
	case "chatgpt":
		return fmt.Errorf("%w: %w", chatgpt.ErrQuotaExhausted, cause)

	case "grok":
		return fmt.Errorf("%w: %w", grok.ErrQuotaExhausted, cause)

	case "dola":
		return fmt.Errorf("%w: %w", dola.ErrQuotaExhausted, cause)

	default:
		return fmt.Errorf("%w: %v", ErrProviderQuota, cause)
	}
}

// refreshDispatchQuota reconciles unknown costs and explicit billing proofs.
// Known costs settle the existing local reservation without a second request.
func (s *V1Service) refreshDispatchQuota(parent context.Context, pool string, token model.TokenAccount, kind string) *float64 {
	plan, ok := dispatchPlanFromContext(parent)
	if !ok || strings.TrimSpace(plan.Route.QuotaBucketKey) == "" {
		return nil
	}
	if pool == "dola" || pool == "chatgpt" && kind != "image" || pool == "grok" && kind == "text" {
		return nil
	}
	probeTimeout := 30 * time.Second
	if pool == "chatgpt" {
		probeTimeout = 10 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), probeTimeout)
	defer cancel()
	reason := repo.QuotaProbeUsage
	if force, _ := parent.Value(quotaVerificationKey{}).(bool); force {
		reason = repo.QuotaProbeVerification
	}
	data, err := runQuotaRefresh(probeCtx, s.models.Quotas(), pool, token.ID, reason, nil, func(probeCtx context.Context) (map[string]any, error) {
		var data map[string]any
		var err error
		switch pool {
		case "adobe":
			if s.adobe != nil {
				data, err = s.adobe.FetchCreditsBalance(probeCtx, token.Value)
			}
		case "byteplus":
			if s.byteplus != nil {
				data, err = s.byteplus.FetchCreditsBalance(probeCtx, token.Value)
			}
		case "chatgpt":
			if s.chatgpt != nil && kind == "image" {
				data, err = s.chatgpt.FetchImageQuota(probeCtx, token.Value)
			}

		case "grok":
			if s.grok != nil && kind != "text" {
				data, err = s.grok.FetchCreditsBalance(probeCtx, token.Value)
			}

		}
		return data, err
	})
	if err != nil || data == nil || boolValueWithDefault(data["unknown"], false) || boolValueWithDefault(data["auth_failed"], false) {
		return nil
	}
	remaining, hasRemaining := anyFloat(data["remaining"])
	if !hasRemaining {
		return nil
	}
	var total *float64
	if value, exists := anyFloat(data["total"]); exists {
		total = &value
	}
	metaPatch := map[string]any{"cached_quota_at": int(time.Now().Unix())}
	if total != nil {
		metaPatch["cached_quota_total"] = canonicalQuotaNumber(*total)
	}
	if used, exists := anyFloat(data["used"]); exists {
		metaPatch["cached_quota_used"] = canonicalQuotaNumber(used)
	}
	patch := map[string]any{}
	if pool == "chatgpt" {
		metaPatch["quota_probe_unknown"] = false
		metaPatch["quota_probe_error"] = nil
		if remaining <= 0 {
			patch["status"] = gorm.Expr("CASE WHEN status = 'active' THEN 'quota' ELSE status END")
		}
		if strings.TrimSpace(token.CachedQuotaResetAfter) == "" {
			patch["cached_quota_reset_after"] = gorm.Expr("COALESCE(NULLIF(cached_quota_reset_after, ''), ?)", time.Now().UTC().Truncate(24*time.Hour).Add(24*time.Hour).Format(time.RFC3339))
		}
	}
	if rawReset := strings.TrimSpace(stringValue(data["reset_after"])); rawReset != "" {
		patch["cached_quota_reset_after"] = rawReset
	} else if rawReset := strings.TrimSpace(stringValue(data["available_until"])); rawReset != "" {
		patch["cached_quota_reset_after"] = rawReset
	}
	tokenErr := s.tokens.UpdateMergingMeta(context.WithoutCancel(parent), pool, token.ID, metaPatch, patch)
	recordBookkeepingError("refresh quota account metadata", tokenErr)
	return &remaining
}

func shouldMarkAccountQuota(err error) bool {
	return !errors.Is(err, errAccountTaskQuota)
}

func poolAccountConcurrency(pool string, item model.TokenAccount) int {
	if item.Concurrency > 0 {
		return min(item.Concurrency, 20)
	}
	if pool == "grok" {
		return grokConcurrencyPerAccount
	}
	if pool == "adobe" {
		if total, ok := jsonMapInt(item.Meta, "cached_quota_total"); ok && total >= 10000 {
			return adobePointsConcurrencyPerAccount
		}
	}
	return 1
}

// markTokenFailure applies Python mark_bad semantics for a failed generation
// attempt against a pool token. It always bumps fail counters; the status side
// effects depend on the failure reason and the provider/pool.
//
//   - quota:  status="quota"; when no cached_quota_reset_after is present, set
//     quota_recover_at to next UTC midnight so the maintenance loop can revive it.
//   - auth on chatgpt: status="disabled" + dead=true (the access token IS the
//     credential; a 401 means it's dead).
//   - auth on adobe: NOT disabled/dead — the access token auto-refreshes from the
//     cookie, so rotate for this request and let the refresh loop mint a new one.
//   - other (non-auth/non-quota): NEITHER pool is auto-disabled — accounts stay
//     active/green and fails is tracked only for rotation ordering.
func (s *V1Service) markTokenFailure(ctx context.Context, pool string, token model.TokenAccount, kind string, isAuth, isQuota bool) {
	patch := map[string]any{
		"last_used_at": time.Now(),
		"fail_total":   gorm.Expr("fail_total + 1"),
		"fails":        gorm.Expr("fails + 1"),
	}
	switch {
	case isQuota:
		// Adobe quota is per-kind: a video-quota error must not block image
		// requests (and vice-versa). Flag only the failing kind, and only sink
		// the account into the shared "quota" waiting status once BOTH kinds are
		// limited. Other pools (chatgpt) are single-kind, so they go straight to
		// "quota" as before.
		if pool == "adobe" {
			imageLimited := token.ImageLimited
			videoLimited := token.VideoLimited
			if kind == "video" {
				videoLimited = true
				patch["video_limited"] = true
			} else {
				imageLimited = true
				patch["image_limited"] = true
			}
			if imageLimited && videoLimited {
				patch["status"] = "quota"
			}
		} else {
			patch["status"] = "quota"
		}
		if strings.TrimSpace(token.CachedQuotaResetAfter) == "" {
			recoverAt := time.Unix((time.Now().Unix()/86400+1)*86400, 0).UTC()
			patch["quota_recover_at"] = &recoverAt
		}
	case isAuth:
		// Adobe auth failures are NOT disabling: the access token refreshes from
		// the cookie. ChatGPT/BytePlus/Runway credentials cannot be refreshed by
		// this request path, so a definitive auth failure disables the account.
		// grok is intentionally excluded: a grok sso can momentarily 401 while
		// still valid (upstream blip / proxy / anti-bot), so an auth failure just
		// fails over for this request without permanently killing the account.
		if pool == "chatgpt" || pool == "byteplus" {
			patch["status"] = "disabled"
			patch["dead"] = true
		}
	default:
		// Neither pool is auto-disabled on generic (non-auth / non-quota) failures
		// — the account usually still works, so it stays active (green). fails is
		// only tracked for rotation ordering. (A chatgpt *auth* failure still marks
		// the token dead in the isAuth case above; that is a genuinely dead token.)
	}
	_, updateErr := s.tokens.Update(ctx, pool, token.ID, patch)
	recordBookkeepingError("record account failure", updateErr)
}

// markTokenUpstreamFailure records a failure caused by the provider itself
// (overload / 5xx / circuit open) WITHOUT touching the account's own health
// counters: during an upstream outage every account fails identically, so
// charging it to fails/fail_total makes a perfectly healthy pool look dead in
// the admin UI (and drowns real per-account problems in noise).
func (s *V1Service) markTokenUpstreamFailure(ctx context.Context, pool string, token model.TokenAccount) {
	_, updateErr := s.tokens.Update(ctx, pool, token.ID, map[string]any{
		"last_used_at":   time.Now(),
		"upstream_fails": gorm.Expr("upstream_fails + 1"),
	})
	recordBookkeepingError("record upstream failure", updateErr)
}

// markTokenDead disables an account and marks it dead on a fatal upstream error
// (a non-overload temporary Adobe failure that ops policy treats as account death).
func (s *V1Service) markTokenDead(ctx context.Context, pool string, token model.TokenAccount, kind string) {
	_, updateErr := s.tokens.Update(ctx, pool, token.ID, map[string]any{
		"last_used_at": time.Now(),
		"fail_total":   gorm.Expr("fail_total + 1"),
		"fails":        gorm.Expr("fails + 1"),
		"status":       "disabled",
		"dead":         true,
	})
	recordBookkeepingError("mark account dead", updateErr)
}

// nextCursor returns the pool's current round-robin position and advances it by
// one. Concurrent callers get different cursor values; changing candidate
// groups can still overlap, so the atomic account gate enforces capacity.
//
// The counter lives in Redis so the rotation survives restarts: with a
// per-process counter every deploy reset it to 0 and the scheduler kept
// re-picking the head of the account list, leaving most of a large pool idle.
// The in-memory fallback (Redis down/unset) starts at a random offset for the
// same reason.
func (s *V1Service) nextCursor(pool string) uint64 {
	if n, ok := s.conc.NextCursor(context.Background(), pool); ok {
		return n
	}
	v, _ := s.tokenCursors.LoadOrStore(pool, randomCursor())
	return atomic.AddUint64(v.(*uint64), 1) - 1
}

func randomCursor() *uint64 {
	start := rand.Uint64()
	return &start
}

// coolDownAccount demotes an account in its pool's rotation for a short window
// after an upstream failure (see acctCooldowns).
func (s *V1Service) coolDownAccount(pool, accountID string) {
	if accountID == "" {
		return
	}
	s.acctCooldowns.Store(pool+":"+accountID, time.Now().Add(accountFailureCooldown))
}

func (s *V1Service) coolDownAccountWithToken(pool string, token model.TokenAccount) {
	s.coolDownAccount(pool, token.ID)
	s.rotateAccountProxy(pool, token)
}

func (s *V1Service) rotateAccountProxy(pool string, token model.TokenAccount) {
	switch pool {
	case "chatgpt":
		if s.chatgpt != nil {
			s.chatgpt.RotateProxySession(token.Value)
		}
	case "grok":
		if s.grok != nil {
			s.grok.RotateProxySession(token.Value)
		}
	case "dola":
		if s.dola != nil {
			s.dola.RotateProxySession(token.ID)
		}
	case "adobe":
		if s.adobe != nil {
			s.adobe.RotateProxySession(token.Value)
		}
	case "byteplus":
		if s.byteplus != nil {
			s.byteplus.RotateProxySession(token.Value)
		}
	}
}

func (s *V1Service) accountCooling(pool, accountID string) bool {
	key := pool + ":" + accountID
	v, ok := s.acctCooldowns.Load(key)
	if !ok {
		return false
	}
	until, _ := v.(time.Time)
	if time.Now().Before(until) {
		return true
	}
	s.acctCooldowns.Delete(key)
	return false
}

func (s *V1Service) rotateRoundRobin(pool string, items []model.TokenAccount) {
	if len(items) <= 1 {
		return
	}
	// Accounts that just failed upstream are demoted behind the healthy ones (but
	// never removed — they're still tried if everything else is busy/cooling).
	// Weight = priority: higher-weight accounts come first, so the scheduler tries
	// them before lower-weight ones (and only falls through when they're at their
	// concurrency cap). Within the SAME cooling state and weight all accounts are
	// equal, so they're rotated by the pool cursor for even distribution.
	cooling := make(map[string]bool, len(items))
	for _, item := range items {
		cooling[item.ID] = s.accountCooling(pool, item.ID)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if cooling[items[i].ID] != cooling[items[j].ID] {
			return !cooling[items[i].ID]
		}
		if items[i].Weight != items[j].Weight {
			return items[i].Weight > items[j].Weight
		}
		return items[i].ID < items[j].ID
	})
	start := s.nextCursor(pool)
	for i := 0; i < len(items); {
		j := i + 1
		for j < len(items) && items[j].Weight == items[i].Weight && cooling[items[j].ID] == cooling[items[i].ID] {
			j++
		}
		if g := j - i; g > 1 {
			off := int(start % uint64(g))
			if off != 0 {
				grp := items[i:j]
				rot := make([]model.TokenAccount, 0, g)
				rot = append(rot, grp[off:]...)
				rot = append(rot, grp[:off]...)
				copy(grp, rot)
			}
		}
		i = j
	}
}
