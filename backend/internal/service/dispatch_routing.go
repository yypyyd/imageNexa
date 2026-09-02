package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"backend/internal/model"
	"backend/internal/provider/adobe"
	"backend/internal/provider/byteplus"
	"backend/internal/provider/chatgpt"
	"backend/internal/provider/custom"
	"backend/internal/provider/grok"
	"backend/internal/provider/oreate"
	"backend/internal/provider/runway"
	"backend/internal/repo"
	"gorm.io/gorm"
)

type dispatchContextKey string

const dispatchRouteKey dispatchContextKey = "model-route"

type dispatchPlan struct {
	Route        model.ModelRoute
	Cost         float64
	QuotaTracked bool
}

func withDispatchRoute(ctx context.Context, route model.ModelRoute, cost float64) context.Context {
	policy, ok := model.DecodeQuotaCostPolicy(route.QuotaCosts)
	tracked := ok && policy.Mode != "unmetered" && strings.TrimSpace(route.QuotaBucketKey) != ""
	return context.WithValue(ctx, dispatchRouteKey, dispatchPlan{Route: route, Cost: maxFloat(0, cost), QuotaTracked: tracked})
}

func withDispatchCost(ctx context.Context, cost float64) context.Context {
	plan, ok := dispatchPlanFromContext(ctx)
	if !ok {
		return ctx
	}
	plan.Cost = maxFloat(0, cost)
	return context.WithValue(ctx, dispatchRouteKey, plan)
}

func dispatchPlanFromContext(ctx context.Context) (dispatchPlan, bool) {
	plan, ok := ctx.Value(dispatchRouteKey).(dispatchPlan)
	return plan, ok && strings.TrimSpace(plan.Route.ID) != ""
}

func dispatchRouteFromContext(ctx context.Context) string {
	plan, _ := dispatchPlanFromContext(ctx)
	return strings.TrimSpace(plan.Route.ID)
}

// matchingRoutes returns the capability-matched routes for a canonical model,
// minus any whose provider pool an administrator has switched off. Every
// dispatch path (text, image, video and the preflight availability probe) goes
// through here, so a disabled provider is never scheduled anywhere.
func (s *V1Service) matchingRoutes(ctx context.Context, logicalID string, req model.RouteRequirements) ([]model.ModelRoute, error) {
	routes, err := s.models.Routes().MatchRoutes(ctx, logicalID, req)
	if err != nil {
		return nil, err
	}
	enabled, err := filterEnabledProviderRoutes(ctx, s.settings, routes)
	if err != nil {
		return nil, err
	}
	// Capability matching found routes, but the administrator closed every pool
	// behind them. Report that precisely instead of blaming the request.
	if len(routes) > 0 && len(enabled) == 0 {
		return nil, ErrProviderDisabled
	}
	return enabled, nil
}

func (s *V1Service) routeHasAccount(ctx context.Context, route model.ModelRoute, kind string) (bool, error) {
	var items []model.TokenAccount
	var err error
	if route.Provider == "custom" {
		items, err = s.customActive(ctx, route.LogicalModelID)
	} else {
		items, err = s.tokens.ListByPool(ctx, route.Provider)
	}
	if err != nil {
		return false, err
	}
	active := make([]model.TokenAccount, 0, len(items))
	for _, item := range items {
		if item.Dead || strings.TrimSpace(item.Value) == "" {
			continue
		}
		if kind == "text" && (route.Provider == "chatgpt" || route.Provider == "grok") {
			if item.Status != "active" && item.Status != "quota" {
				continue
			}
		} else if item.Status != "active" {
			continue
		}
		if kind == "image" && item.ImageLimited || kind == "video" && item.VideoLimited {
			continue
		}
		active = append(active, item)
	}
	// The caller already attached this normalized request's route cost. Keep it
	// through the availability probe so a known-insufficient route is not chosen
	// merely because it has an otherwise active account.
	active, err = s.routeAccounts(ctx, route, active)
	return len(active) > 0, err
}

func (s *V1Service) firstAvailableRoute(ctx context.Context, logicalID, kind string, req model.RouteRequirements) (*model.ModelRoute, error) {
	routes, err := s.matchingRoutes(ctx, logicalID, req)
	if err != nil {
		return nil, err
	}
	if len(routes) == 0 {
		return nil, ErrUnsupportedParams
	}
	for _, route := range routes {
		routeCtx := withDispatchRoute(ctx, route, defaultRouteCost(route, req))
		ok, accountErr := s.routeHasAccount(routeCtx, route, kind)
		if accountErr != nil {
			return nil, accountErr
		}
		if ok {
			copy := route
			return &copy, nil
		}
	}
	return nil, ErrNoProviderAccount
}

func (s *V1Service) routeAccounts(ctx context.Context, route model.ModelRoute, accounts []model.TokenAccount) ([]model.TokenAccount, error) {
	type candidate struct {
		account   model.TokenAccount
		binding   model.AccountModelRoute
		bucketKey string
		remaining float64
		known     bool
		available int64
		slotsSeen bool
	}
	plan, _ := dispatchPlanFromContext(ctx)
	accountIDs := make([]string, 0, len(accounts))
	for _, account := range accounts {
		if strings.TrimSpace(account.ID) != "" {
			accountIDs = append(accountIDs, account.ID)
		}
	}
	bindings, err := s.models.Routes().AccountRoutesForRoute(ctx, accountIDs, route.ID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now := time.Now()
	candidates := make([]candidate, 0, len(accounts))
	bucketKeys := make([]string, 0, len(accounts))
	for _, account := range accounts {
		binding, exists := bindings[account.ID]
		if !exists {
			continue
		}
		if !binding.Enabled || !binding.Entitled || binding.CooldownUntil != nil && binding.CooldownUntil.After(now) {
			continue
		}
		item := candidate{account: account, binding: binding}
		bucketKey := strings.TrimSpace(binding.QuotaBucketKey)
		if bucketKey == "" {
			bucketKey = strings.TrimSpace(route.QuotaBucketKey)
		}
		if authoritativeBucket, _, scoped := providerSnapshotScope(route.Provider); scoped && authoritativeBucket != bucketKey {
			// The provider's available probe covers a different product surface
			// (for example ChatGPT image_gen versus text chat). Ignore stale or
			// accidentally created snapshots rather than blocking an unrelated route.
			bucketKey = ""
		}
		if bucketKey != "" {
			item.bucketKey = bucketKey
			bucketKeys = append(bucketKeys, bucketKey)
		}
		candidates = append(candidates, item)
	}

	// Quota snapshots are read in one query even when account-specific bindings
	// point at different buckets (Adobe image/video intentionally share one).
	snapshots, err := s.models.Quotas().GetMany(ctx, accountIDs, bucketKeys)
	if err != nil {
		return nil, err
	}
	quotaEligible := candidates[:0]
	for _, item := range candidates {
		if item.bucketKey != "" {
			if bucket, exists := snapshots[repo.QuotaSnapshotKey(item.account.ID, item.bucketKey)]; exists && bucket.Remaining != nil {
				item.remaining, item.known = *bucket.Remaining, true
				if !quotaCanServe(item.remaining, item.known, plan) {
					continue
				}
			}
		}
		quotaEligible = append(quotaEligible, item)
	}
	candidates = quotaEligible

	// Observe all account gates in one Redis pipeline. A failed observation is
	// unknown (not zero): final admission remains the atomic Acquire in the pool.
	concurrencyKeys := make([]string, 0, len(candidates))
	for _, item := range candidates {
		concurrencyKeys = append(concurrencyKeys, "conc:a:"+item.account.ID)
	}
	activeCounts, slotsSeen := s.conc.ActiveCounts(ctx, concurrencyKeys)
	if slotsSeen {
		slotEligible := candidates[:0]
		for _, item := range candidates {
			maximum := int64(poolAccountConcurrency(route.Provider, item.account))
			active := activeCounts["conc:a:"+item.account.ID]
			item.available, item.slotsSeen = maximum-active, true
			if item.available <= 0 {
				continue
			}
			slotEligible = append(slotEligible, item)
		}
		candidates = slotEligible
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		// Administrator weight is the primary scheduling preference. Within the
		// same weight, prefer accounts that can absorb more concurrent work, then
		// authoritative quota snapshots and best-fit remaining allowance.
		if left.account.Weight != right.account.Weight {
			return left.account.Weight > right.account.Weight
		}
		if left.slotsSeen && right.slotsSeen && left.available != right.available {
			return left.available > right.available
		}
		if left.known != right.known {
			return left.known
		}
		if left.binding.ConsecutiveFails != right.binding.ConsecutiveFails {
			return left.binding.ConsecutiveFails < right.binding.ConsecutiveFails
		}
		if left.known && left.remaining != right.remaining {
			return left.remaining < right.remaining
		}
		if left.account.LastUsedAt == nil || right.account.LastUsedAt == nil {
			return left.account.LastUsedAt == nil && right.account.LastUsedAt != nil
		}
		if !left.account.LastUsedAt.Equal(*right.account.LastUsedAt) {
			return left.account.LastUsedAt.Before(*right.account.LastUsedAt)
		}
		return left.account.ID < right.account.ID
	})
	out := make([]model.TokenAccount, 0, len(candidates))
	for _, item := range candidates {
		out = append(out, item.account)
	}
	return out, nil
}

func quotaCanServe(remaining float64, known bool, plan dispatchPlan) bool {
	if !known || !plan.QuotaTracked {
		return true
	}
	if remaining <= 1e-9 {
		return false
	}
	return plan.Cost <= 0 || remaining+1e-9 >= plan.Cost
}

func maxFloat(left, right float64) float64 {
	if left > right {
		return left
	}
	return right
}

func (s *V1Service) dispatchImageRoutes(ctx context.Context, eventID string, logical *model.ModelConfig, in V1ImageRequest, ratio, resolution string, noStore bool) ([]byte, string, error) {
	operation := "generation"
	if len(in.ReferenceImages) > 0 {
		operation = "edit"
	}
	requirements := model.RouteRequirements{Operation: operation, Ratio: ratio, Resolution: resolution, ReferenceImages: len(in.ReferenceImages)}
	routes, err := s.matchingRoutes(ctx, logical.ID, requirements)
	if err != nil {
		return nil, "", err
	}
	if len(routes) == 0 {
		return nil, "", ErrUnsupportedParams
	}
	var lastErr error
	for _, route := range routes {
		routeCtx := withDispatchRoute(ctx, route, defaultRouteCost(route, requirements))
		available, accountErr := s.routeHasAccount(routeCtx, route, "image")
		if accountErr != nil {
			return nil, "", accountErr
		}
		if !available {
			lastErr = ErrNoProviderAccount
			continue
		}
		item, configErr := s.models.Routes().RouteConfig(ctx, route)
		if configErr != nil {
			return nil, "", configErr
		}
		_ = s.events.SetProvider(context.WithoutCancel(ctx), eventID, route.Provider)
		var data []byte
		var url string
		switch route.Provider {
		case "adobe":
			data, url, lastErr = s.generateAdobeImage(routeCtx, eventID, item, in, ratio, resolution, noStore)
		case "byteplus":
			data, url, lastErr = s.generateBytePlusImage(routeCtx, eventID, item, in, ratio, resolution, noStore)
		case "chatgpt":
			data, url, lastErr = s.generateChatGPTImage(routeCtx, eventID, item, in, ratio, resolution, noStore)
		case "runway":
			data, url, lastErr = s.generateRunwayImage(routeCtx, eventID, item, in, ratio, resolution, noStore)
		case "grok":
			data, url, lastErr = s.generateGrokImage(routeCtx, eventID, item, in, ratio, noStore)
		case "custom":
			data, url, lastErr = s.generateCustomImage(routeCtx, eventID, item, in, ratio, resolution, noStore)
		default:
			lastErr = ErrProviderUnsupported
		}
		if lastErr == nil {
			return data, url, nil
		}
		if noRouteFailover(lastErr) {
			return nil, "", lastErr
		}
		if !routeFailoverSafe(lastErr) {
			return nil, "", lastErr
		}
	}
	if lastErr == nil {
		lastErr = ErrNoProviderAccount
	}
	return nil, "", lastErr
}

func (s *V1Service) dispatchVideoRoutes(ctx context.Context, eventID string, logical *model.ModelConfig, in V1VideoRequest, ratio, resolution, duration string, download bool) ([]byte, string, error) {
	requirements := model.RouteRequirements{Operation: "generation", Ratio: ratio, Resolution: resolution, Duration: duration,
		ReferenceImages: len(in.ReferenceImages), ReferenceVideos: len(in.ReferenceVideos), ReferenceAudios: len(in.ReferenceAudios), GenerateAudio: in.GenerateAudio}
	routes, err := s.matchingRoutes(ctx, logical.ID, requirements)
	if err != nil {
		return nil, "", err
	}
	if len(routes) == 0 {
		return nil, "", ErrUnsupportedParams
	}
	var lastErr error
	for _, route := range routes {
		routeCtx := withDispatchRoute(ctx, route, defaultRouteCost(route, requirements))
		available, accountErr := s.routeHasAccount(routeCtx, route, "video")
		if accountErr != nil {
			return nil, "", accountErr
		}
		if !available {
			lastErr = ErrNoProviderAccount
			continue
		}
		item, configErr := s.models.Routes().RouteConfig(ctx, route)
		if configErr != nil {
			return nil, "", configErr
		}
		_ = s.events.SetProvider(context.WithoutCancel(ctx), eventID, route.Provider)
		seconds := parseDurationSeconds(duration)
		var data []byte
		var url string
		switch route.Provider {
		case "adobe":
			data, url, lastErr = s.generateAdobeVideo(routeCtx, eventID, item, in, ratio, resolution, seconds, download)
		case "runway":
			data, url, lastErr = s.generateRunwayVideo(routeCtx, eventID, item, in, ratio, seconds, download)
		case "grok":
			data, url, lastErr = s.generateGrokVideo(routeCtx, eventID, item, in, ratio, resolution, seconds, download)
		case "oreate":
			data, url, lastErr = s.generateOreateVideo(routeCtx, eventID, item, in, ratio, resolution, seconds, download)
		case "custom":
			data, url, lastErr = s.generateCustomVideo(routeCtx, eventID, item, in, ratio, resolution, seconds, download)
		default:
			lastErr = ErrProviderUnsupported
		}
		if lastErr == nil {
			return data, url, nil
		}
		if noRouteFailover(lastErr) {
			return nil, "", lastErr
		}
		if !routeFailoverSafe(lastErr) {
			return nil, "", lastErr
		}
	}
	if lastErr == nil {
		lastErr = ErrNoProviderAccount
	}
	return nil, "", lastErr
}

func defaultRouteCost(route model.ModelRoute, req model.RouteRequirements) float64 {
	cost, _ := evaluateRouteCost(route, req)
	return cost
}

// evaluateRouteCost is the single route-level allowance evaluator. Policies
// are data on ModelRoute rather than provider-name fallbacks, so an unknown
// upstream price is explicit and never silently turned into a fake one-credit
// reservation. Providers with request-specific public price tables evaluate
// the normalized model, resolution, duration, references, and audio here.
func evaluateRouteCost(route model.ModelRoute, req model.RouteRequirements) (float64, bool) {
	policy, ok := model.DecodeQuotaCostPolicy(route.QuotaCosts)
	if !ok || policy.Mode == "unknown" || policy.Mode == "unmetered" {
		return 0, false
	}
	if policy.Mode != "metered" {
		return 0, false
	}
	switch policy.Calculator {
	case "fixed":
		if policy.Value < 0 {
			return 0, false
		}
		return policy.Value, true
	case "per_second":
		seconds := parseDurationSeconds(req.Duration)
		if seconds <= 0 || policy.PerSecond <= 0 {
			return 0, false
		}
		return float64(seconds) * policy.PerSecond, true
	case "byteplus_image":
		providerModel := strings.TrimSpace(route.UpstreamModel)
		if providerModel == "" {
			providerModel = strings.TrimSpace(route.RuntimeModel)
		}
		credits, err := byteplus.RequiredCredits(byteplus.ImageRequest{
			Model: providerModel, Resolution: req.Resolution, Size: req.Resolution,
			AspectRatio: req.Ratio, Quality: upstreamQuality(req.Resolution),
			References: make([][]byte, max(0, req.ReferenceImages)),
		})
		if err != nil || credits < 0 {
			return 0, false
		}
		return credits, true
	case "oreate_seedance":
		// Reference-video pricing depends on the decoded videos' total duration.
		// The Oreate adapter replaces this provisional unknown with its exact cost
		// before entering the account pool; never guess from the reference count.
		if req.ReferenceVideos > 0 {
			return 0, false
		}
		providerModel := strings.TrimSpace(route.UpstreamModel)
		if providerModel == "" {
			providerModel = strings.TrimSpace(route.RuntimeModel)
		}
		credits, err := oreate.SeedanceRequiredCredits(providerModel, req.Resolution, parseDurationSeconds(req.Duration), req.GenerateAudio, 0)
		if err != nil || credits < 0 {
			return 0, false
		}
		return float64(credits), true
	default:
		return 0, false
	}
}

func noRouteFailover(err error) bool {
	return errors.Is(err, byteplus.ErrTaskAccepted) || errors.Is(err, byteplus.ErrTaskSubmissionUnknown)
}

func routeFailoverSafe(err error) bool {
	return errors.Is(err, ErrNoProviderAccount) || errors.Is(err, ErrConcurrencyFull) ||
		errors.Is(err, adobe.ErrEntitlement) || errors.Is(err, adobe.ErrAuth) || errors.Is(err, adobe.ErrQuotaExhausted) || errors.Is(err, adobe.ErrTemporaryUpstream) ||
		errors.Is(err, byteplus.ErrAuth) || errors.Is(err, byteplus.ErrQuotaExhausted) || errors.Is(err, byteplus.ErrTemporaryUpstream) ||
		errors.Is(err, chatgpt.ErrAuth) || errors.Is(err, chatgpt.ErrQuotaExhausted) || errors.Is(err, chatgpt.ErrTemporaryUpstream) ||
		errors.Is(err, runway.ErrAuth) || errors.Is(err, runway.ErrQuotaExhausted) || errors.Is(err, runway.ErrTemporaryUpstream) ||
		errors.Is(err, grok.ErrAuth) || errors.Is(err, grok.ErrQuotaExhausted) || errors.Is(err, grok.ErrTemporaryUpstream) ||
		errors.Is(err, oreate.ErrAuth) || errors.Is(err, oreate.ErrQuotaExhausted) || errors.Is(err, oreate.ErrTemporaryUpstream) ||
		errors.Is(err, custom.ErrAuth) || errors.Is(err, custom.ErrQuotaExhausted) || errors.Is(err, custom.ErrTemporaryUpstream)
}

// runTextRouteFailover keeps route selection separate from account failover.
// Each callback invocation owns one route-specific account pool and dispatch
// context. Once a route returns a response body (including SSE), it is final:
// any later stream failure is accounted by that body's finish callback and is
// never resubmitted to another provider.
func runTextRouteFailover(routes []model.ModelRoute, attempt func(model.ModelRoute) (*V1ChatResponse, error)) (*V1ChatResponse, error) {
	var lastErr error
	for _, route := range routes {
		response, err := attempt(route)
		if err == nil {
			return response, nil
		}
		lastErr = err
		if noRouteFailover(err) || !routeFailoverSafe(err) {
			return nil, err
		}
	}
	if lastErr == nil {
		lastErr = ErrNoProviderAccount
	}
	return nil, lastErr
}

func dispatchFailureClass(err error) (state, class string) {
	switch {
	case err == nil:
		return "succeeded", ""
	case errors.Is(err, byteplus.ErrTaskAccepted):
		return "accepted", "temporary"
	case errors.Is(err, byteplus.ErrTaskSubmissionUnknown):
		return "unknown", "temporary"
	case errors.Is(err, adobe.ErrEntitlement):
		return "failed", "entitlement"
	case errors.Is(err, adobe.ErrAuth), errors.Is(err, byteplus.ErrAuth), errors.Is(err, chatgpt.ErrAuth), errors.Is(err, runway.ErrAuth), errors.Is(err, grok.ErrAuth), errors.Is(err, oreate.ErrAuth), errors.Is(err, custom.ErrAuth):
		return "failed", "auth"
	case errors.Is(err, adobe.ErrQuotaExhausted), errors.Is(err, byteplus.ErrQuotaExhausted), errors.Is(err, chatgpt.ErrQuotaExhausted), errors.Is(err, runway.ErrQuotaExhausted), errors.Is(err, grok.ErrQuotaExhausted), errors.Is(err, oreate.ErrQuotaExhausted), errors.Is(err, custom.ErrQuotaExhausted):
		return "failed", "quota"
	case routeFailoverSafe(err):
		return "failed", "temporary"
	default:
		return "failed", "request"
	}
}
