package service

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"time"

	"backend/internal/model"
	"gorm.io/gorm"
)

const poolSchedulingKey dispatchContextKey = "pool-scheduling"

type poolSchedulingPolicy struct {
	fastFailover bool
	pinnedID     string
	requests     map[string]*poolAccountRequest
}

// One downstream request can revisit a full route. Preserve its failed and
// previously attempted accounts across those visits, not just one pool loop.
type poolAccountRequest struct {
	excluded  map[string]bool
	attempted map[string]struct{}
}

func (policy poolSchedulingPolicy) accountRequest(ctx context.Context, pool string) *poolAccountRequest {
	key := dispatchRouteFromContext(ctx)
	if key == "" {
		key = pool
	}
	if request := policy.requests[key]; request != nil {
		return request
	}
	request := &poolAccountRequest{excluded: make(map[string]bool), attempted: make(map[string]struct{})}
	if policy.requests != nil {
		policy.requests[key] = request
	}
	return request
}

func poolPolicy(ctx context.Context) poolSchedulingPolicy {
	policy, _ := ctx.Value(poolSchedulingKey).(poolSchedulingPolicy)
	return policy
}

func accountDispatchable(account model.TokenAccount, pool, kind, logicalID, pinnedID string, now time.Time) bool {
	if strings.TrimSpace(account.Value) == "" {
		return false
	}
	if pinnedID != "" && account.ID == pinnedID {
		return true // Explicit administrator credential test.
	}
	if account.Dead || (account.Status != "active" && !(kind == "text" && (pool == "chatgpt" || pool == "grok") && account.Status == "quota")) {
		return false
	}
	if kind == "image" && account.ImageLimited || kind == "video" && account.VideoLimited {
		return false
	}
	if pool == "byteplus" && !bytePlusAccountSessionUsable(account, now) {
		return false
	}
	return pool != "custom" || customAccountServes(account, logicalID)
}

func (s *V1Service) refreshPoolAccounts(ctx context.Context, pool, kind string, active []model.TokenAccount, excluded map[string]bool) ([]model.TokenAccount, error) {
	ids := make([]string, 0, len(active))
	for _, account := range active {
		if !excluded[account.ID] {
			ids = append(ids, account.ID)
		}
	}
	fresh, err := s.tokens.ListByIDs(ctx, pool, ids)
	if err != nil {
		return nil, err
	}
	plan, _ := dispatchPlanFromContext(ctx)
	policy := poolPolicy(ctx)
	byID := make(map[string]model.TokenAccount, len(fresh))
	for _, account := range fresh {
		if accountDispatchable(account, pool, kind, plan.Route.LogicalModelID, policy.pinnedID, time.Now()) {
			byID[account.ID] = account
		}
	}
	out := make([]model.TokenAccount, 0, len(fresh))
	for _, previous := range active {
		if account, ok := byID[previous.ID]; ok {
			out = append(out, account)
		}
	}
	return out, nil
}

// revalidateDispatchAccount runs after the concurrency lease is acquired and
// immediately before submission. Being queued does not grant permission to
// ignore a later disablement, credential rotation, or first-admission cooldown.
func (s *V1Service) revalidateDispatchAccount(ctx context.Context, pool, accountID, kind string, retry bool) (model.TokenAccount, error) {
	account, err := s.tokens.Get(ctx, pool, accountID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.TokenAccount{}, ErrNoProviderAccount
	}
	if err != nil {
		return model.TokenAccount{}, err
	}
	plan, routed := dispatchPlanFromContext(ctx)
	if !accountDispatchable(*account, pool, kind, plan.Route.LogicalModelID, poolPolicy(ctx).pinnedID, time.Now()) {
		return model.TokenAccount{}, ErrNoProviderAccount
	}
	if s.settings != nil {
		enabled, err := providerEnabled(ctx, s.settings, pool)
		if err != nil {
			return model.TokenAccount{}, err
		}
		if !enabled {
			return model.TokenAccount{}, ErrNoProviderAccount
		}
	}
	if !routed {
		return *account, nil
	}
	binding, err := s.models.Routes().DispatchBinding(ctx, accountID, plan.Route.ID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.TokenAccount{}, ErrNoProviderAccount
	}
	if err != nil {
		return model.TokenAccount{}, err
	}
	if !binding.Enabled || !binding.Entitled || (!retry && binding.CooldownUntil != nil && binding.CooldownUntil.After(time.Now())) {
		return model.TokenAccount{}, ErrNoProviderAccount
	}
	// A zero/unknown-cost tracked route still needs its known-zero check. For
	// metered requests ReserveWithUnit provides the final atomic check.
	bucketKey := strings.TrimSpace(binding.QuotaBucketKey)
	if bucketKey == "" {
		bucketKey = strings.TrimSpace(plan.Route.QuotaBucketKey)
	}
	if authoritative, _, scoped := providerSnapshotScope(pool); scoped && authoritative != bucketKey {
		bucketKey = ""
	}
	if plan.QuotaTracked && bucketKey != "" {
		bucket, err := s.models.Quotas().Get(ctx, accountID, bucketKey)
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return model.TokenAccount{}, err
		}
		if err == nil && bucket.Remaining != nil && !quotaCanServe(*bucket.Remaining, true, plan) {
			return model.TokenAccount{}, providerQuotaError(pool, errAccountTaskQuota)
		}
	}
	return *account, nil
}

func accountGateKeys(accounts []model.TokenAccount) []string {
	keys := make([]string, 0, len(accounts))
	for _, account := range accounts {
		keys = append(keys, "conc:a:"+account.ID)
	}
	return keys
}

func waitAccountQueue(ctx context.Context, deadline time.Time) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 || ctx.Err() != nil {
		return false
	}
	// Jitter prevents queued requests from polling in synchronized waves.
	delay := providerAccountQueuePoll/2 + time.Duration(rand.Int63n(int64(providerAccountQueuePoll)))
	timer := time.NewTimer(min(delay, remaining))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// runMediaRouteFailover probes every capable route before waiting on capacity.
// Only routes that were full are revisited; a provider failure never becomes
// an unbounded cross-route retry loop. Single routes retain their own retry
// budget, including the BytePlus verified-beta same-route exception.
func runMediaRouteFailover(ctx context.Context, routes []model.ModelRoute, pinnedID string, attempt func(context.Context, model.ModelRoute) ([]byte, string, error)) ([]byte, string, error) {
	policy := poolSchedulingPolicy{fastFailover: len(routes) > 1 && pinnedID == "", pinnedID: pinnedID, requests: make(map[string]*poolAccountRequest)}
	ctx = context.WithValue(ctx, poolSchedulingKey, policy)
	deadline := time.Now().Add(providerAccountQueueWait)
	pending := append([]model.ModelRoute(nil), routes...)
	var lastErr error
	for len(pending) > 0 {
		busy := make([]model.ModelRoute, 0, len(pending))
		for _, route := range pending {
			if ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			data, url, err := attempt(ctx, route)
			if err == nil {
				return data, url, nil
			}
			lastErr = err
			if noRouteFailover(err) || !routeFailoverSafe(err) {
				return nil, "", err
			}
			if policy.fastFailover && errors.Is(err, ErrConcurrencyFull) {
				busy = append(busy, route)
			}
		}
		if len(busy) == 0 {
			break
		}
		if !waitAccountQueue(ctx, deadline) {
			if ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			return nil, "", ErrConcurrencyFull
		}
		pending = busy
	}
	if lastErr == nil {
		lastErr = ErrNoProviderAccount
	}
	return nil, "", lastErr
}
