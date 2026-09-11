package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"backend/internal/config"
	"backend/internal/model"
	"backend/internal/repo"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AdminConsoleService exposes the deliberately small 2API control plane. It
// owns no customer, billing, invite, recharge, or playground concepts.
type AdminConsoleService struct {
	cfg      *config.Config
	db       *gorm.DB
	models   *repo.ModelRepository
	tokens   *repo.TokenRepository
	tokenSvc *TokenService
	settings *repo.SiteSettingRepository
}

type AccountListFilter struct {
	Query    string
	Provider string
	Status   string
	ModelID  string
	Limit    int
	Offset   int
}

type LogListFilter struct {
	CredentialID string
	Model        string
	Kind         string
	Status       string
	Source       string
	Query        string
	Artifacts    bool
	Limit        int
	Offset       int
}

type AccountImportInput struct {
	ID         string
	Provider   string
	Label      string
	Credential any
}

var adminAccountProviders = SchedulableProviders()

func NewAdminConsoleService(cfg *config.Config, db *gorm.DB, models *repo.ModelRepository, tokens *repo.TokenRepository, tokenSvc *TokenService, settings *repo.SiteSettingRepository) *AdminConsoleService {
	return &AdminConsoleService{cfg: cfg, db: db, models: models, tokens: tokens, tokenSvc: tokenSvc, settings: settings}
}

// Overview restores the retained operational dashboard without bringing back
// the retired customer, billing, invite, or payment analytics. Aggregation is
// performed in SQL so log retention volume does not silently truncate charts.
func (s *AdminConsoleService) Overview(ctx context.Context) (map[string]any, error) {
	cutoff := time.Now().Add(-24 * time.Hour)
	type groupedCount struct {
		Name  string `gorm:"column:name"`
		Count int64  `gorm:"column:count"`
	}
	var statuses, kinds []groupedCount
	if err := s.db.WithContext(ctx).Model(&model.EventLog{}).
		Select("status AS name, COUNT(*) AS count").Where("ts >= ?", cutoff).Group("status").Scan(&statuses).Error; err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Model(&model.EventLog{}).
		Select("kind AS name, COUNT(*) AS count").Where("ts >= ?", cutoff).Group("kind").Scan(&kinds).Error; err != nil {
		return nil, err
	}
	window := map[string]any{"total": int64(0), "success": int64(0), "failed": int64(0), "pending": int64(0), "text": int64(0), "image": int64(0), "video": int64(0), "avg_elapsed_ms": float64(0)}
	for _, row := range statuses {
		window["total"] = window["total"].(int64) + row.Count
		switch row.Name {
		case "success", "succeeded":
			window["success"] = window["success"].(int64) + row.Count
		case "failed":
			window["failed"] = window["failed"].(int64) + row.Count
		default:
			window["pending"] = window["pending"].(int64) + row.Count
		}
	}
	for _, row := range kinds {
		if _, ok := window[row.Name]; ok {
			window[row.Name] = row.Count
		}
	}
	var average struct {
		Value float64 `gorm:"column:value"`
	}
	if err := s.db.WithContext(ctx).Model(&model.EventLog{}).
		Select("COALESCE(AVG(elapsed_ms), 0) AS value").
		Where("ts >= ? AND status IN ? AND elapsed_ms > 0", cutoff, []string{"success", "succeeded"}).Scan(&average).Error; err != nil {
		return nil, err
	}
	window["avg_elapsed_ms"] = average.Value

	type modelUsage struct {
		Model string  `json:"model" gorm:"column:model"`
		Count int64   `json:"count" gorm:"column:count"`
		AvgMS float64 `json:"avg_ms" gorm:"column:avg_ms"`
	}
	var topModels []modelUsage
	if err := s.db.WithContext(ctx).Model(&model.EventLog{}).
		Select("model, COUNT(*) AS count, COALESCE(AVG(NULLIF(elapsed_ms, 0)), 0) AS avg_ms").
		Where("ts >= ? AND model <> ''", cutoff).Group("model").Order("count DESC").Limit(8).Scan(&topModels).Error; err != nil {
		return nil, err
	}
	type failureUsage struct {
		Reason string `json:"reason" gorm:"column:reason"`
		Count  int64  `json:"count" gorm:"column:count"`
	}
	var failures []failureUsage
	if err := s.db.WithContext(ctx).Model(&model.EventLog{}).
		Select("error AS reason, COUNT(*) AS count").
		Where("ts >= ? AND status = ? AND error <> ''", cutoff, "failed").Group("error").Order("count DESC").Limit(8).Scan(&failures).Error; err != nil {
		return nil, err
	}

	type hourlyCount struct {
		Hour  time.Time `gorm:"column:hour"`
		Kind  string    `gorm:"column:kind"`
		Count int64     `gorm:"column:count"`
	}
	var hourlyRows []hourlyCount
	if err := s.db.WithContext(ctx).Model(&model.EventLog{}).
		Select("date_trunc('hour', ts) AS hour, kind, COUNT(*) AS count").
		Where("ts >= ?", cutoff).Group("hour, kind").Order("hour ASC").Scan(&hourlyRows).Error; err != nil {
		return nil, err
	}
	nowHour := time.Now().UTC().Truncate(time.Hour)
	hourly := make([]map[string]any, 24)
	byHour := make(map[string]map[string]any, 24)
	for index := 0; index < 24; index++ {
		hour := nowHour.Add(time.Duration(index-23) * time.Hour)
		key := hour.Format(time.RFC3339)
		bucket := map[string]any{"hour": key, "text": int64(0), "image": int64(0), "video": int64(0)}
		hourly[index] = bucket
		byHour[key] = bucket
	}
	for _, row := range hourlyRows {
		key := row.Hour.UTC().Truncate(time.Hour).Format(time.RFC3339)
		if bucket := byHour[key]; bucket != nil {
			if _, ok := bucket[row.Kind]; ok {
				bucket[row.Kind] = row.Count
			}
		}
	}
	providerHealth, err := s.CountAccountHealthByProvider(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"last_24h": window, "hourly": hourly, "top_models": topModels,
		"failures": failures, "provider_health": providerHealth,
	}, nil
}

func (s *AdminConsoleService) ListLogicalModels(ctx context.Context) ([]map[string]any, error) {
	items, err := s.models.Routes().ListLogical(ctx, false)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		routes, routeErr := s.models.Routes().ListRoutes(ctx, item.ID, false)
		if routeErr != nil {
			return nil, routeErr
		}
		routeRows := make([]map[string]any, 0, len(routes))
		for _, route := range routes {
			var accountCount, healthyCount int64
			accountQuery := s.db.WithContext(ctx).Model(&model.TokenAccount{}).
				Where("pool = ?", route.Provider)
			if s.db.Migrator().HasTable(&model.AccountModelRoute{}) {
				accountQuery = accountQuery.Joins("JOIN account_model_routes amr ON amr.account_id = provider_accounts.id").
					Where("amr.model_route_id = ? AND amr.enabled = ? AND amr.entitled = ?", route.ID, true, true)
			}
			_ = accountQuery.Count(&accountCount).Error
			_ = accountQuery.Where("provider_accounts.status = ? AND provider_accounts.dead = ?", "active", false).Count(&healthyCount).Error
			routeRows = append(routeRows, map[string]any{
				"id": route.ID, "provider": route.Provider, "upstream_model": route.UpstreamModel,
				"enabled": route.Enabled, "priority": route.Priority, "weight": route.Weight,
				"capabilities": json.RawMessage(route.Capabilities), "account_count": accountCount,
				"healthy_accounts": healthyCount,
			})
		}
		out = append(out, map[string]any{
			"id": item.ID, "kind": item.Kind, "name": item.Name, "enabled": item.Enabled,
			"created_at": item.CreatedAt, "routes": routeRows,
		})
	}
	return out, nil
}

func (s *AdminConsoleService) UpdateLogicalModel(ctx context.Context, id string, enabled bool) (map[string]any, error) {
	item, err := s.models.Update(ctx, strings.TrimSpace(id), map[string]any{"enabled": enabled})
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": item.ID, "enabled": item.Enabled}, nil
}

func (s *AdminConsoleService) UpdateLogicalRoute(ctx context.Context, logicalID, routeID string, enabled bool) (map[string]any, error) {
	routes, err := s.models.Routes().ListRoutes(ctx, logicalID, false)
	if err != nil {
		return nil, err
	}
	found := false
	for _, route := range routes {
		if route.ID == routeID {
			found = true
			break
		}
	}
	if !found {
		return nil, gorm.ErrRecordNotFound
	}
	item, err := s.models.Routes().UpdateRoute(ctx, routeID, map[string]any{"enabled": enabled})
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": item.ID, "enabled": item.Enabled}, nil
}

func (s *AdminConsoleService) ListAccounts(ctx context.Context, filter AccountListFilter) ([]map[string]any, int64, error) {
	query := s.db.WithContext(ctx).Model(&model.TokenAccount{}).Where("pool IN ?", adminAccountProviders)
	if modelID := strings.TrimSpace(filter.ModelID); modelID != "" {
		query = query.Where(`EXISTS (
			SELECT 1 FROM account_model_routes filter_amr
			JOIN model_routes filter_mr ON filter_mr.id = filter_amr.model_route_id
			WHERE filter_amr.account_id = provider_accounts.id
			  AND filter_mr.logical_model_id = ? AND filter_mr.enabled = ?
			  AND filter_amr.enabled = ? AND filter_amr.entitled = ?
		)`, modelID, true, true, true)
	}
	if requested := strings.TrimSpace(filter.Provider); requested != "" {
		provider := normalizeAdminProvider(requested)
		if provider == "" {
			return []map[string]any{}, 0, nil
		}
		query = query.Where("pool = ?", provider)
	}
	if status := strings.TrimSpace(filter.Status); status != "" {
		query = query.Where("status = ?", status)
	}
	if term := strings.TrimSpace(filter.Query); term != "" {
		like := "%" + term + "%"
		query = query.Where("id ILIKE ? OR pool ILIKE ? OR account_email ILIKE ? OR account_display_name ILIKE ?", like, like, like, like)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	limit, offset := normalizePage(filter.Limit, filter.Offset)
	var accounts []model.TokenAccount
	if err := query.Order("pool asc, weight desc, created_at desc").Limit(limit).Offset(offset).Find(&accounts).Error; err != nil {
		return nil, 0, err
	}
	rows := make([]map[string]any, 0, len(accounts))
	for _, account := range accounts {
		row, err := s.accountRow(ctx, account)
		if err != nil {
			return nil, 0, err
		}
		rows = append(rows, row)
	}
	return rows, total, nil
}

func (s *AdminConsoleService) CountAccountsByProvider(ctx context.Context) (map[string]int64, error) {
	type providerCount struct {
		Provider string `gorm:"column:provider"`
		Count    int64  `gorm:"column:count"`
	}
	var rows []providerCount
	if err := s.db.WithContext(ctx).Model(&model.TokenAccount{}).
		Select("pool AS provider, COUNT(*) AS count").
		Where("pool IN ?", adminAccountProviders).
		Group("pool").Scan(&rows).Error; err != nil {
		return nil, err
	}
	counts := make(map[string]int64, len(adminAccountProviders))
	for _, provider := range adminAccountProviders {
		counts[provider] = 0
	}
	for _, row := range rows {
		counts[row.Provider] = row.Count
	}
	return counts, nil
}

func (s *AdminConsoleService) CountAccountHealthByProvider(ctx context.Context) (map[string]map[string]int64, error) {
	type healthCount struct {
		Provider string `gorm:"column:provider"`
		Status   string `gorm:"column:status"`
		Dead     bool   `gorm:"column:dead"`
		Count    int64  `gorm:"column:count"`
	}
	var rows []healthCount
	if err := s.db.WithContext(ctx).Model(&model.TokenAccount{}).
		Select("pool AS provider, status, dead, COUNT(*) AS count").
		Where("pool IN ?", adminAccountProviders).
		Group("pool, status, dead").Scan(&rows).Error; err != nil {
		return nil, err
	}
	counts := make(map[string]map[string]int64, len(adminAccountProviders))
	for _, provider := range adminAccountProviders {
		counts[provider] = map[string]int64{"active": 0, "quota": 0, "disabled": 0, "dead": 0, "pending": 0}
	}
	for _, row := range rows {
		bucket := row.Status
		if row.Dead || row.Status == "auth_error" {
			bucket = "dead"
		}
		if _, ok := counts[row.Provider][bucket]; !ok {
			bucket = "pending"
		}
		counts[row.Provider][bucket] += row.Count
	}
	return counts, nil
}

func (s *AdminConsoleService) accountRow(ctx context.Context, account model.TokenAccount) (map[string]any, error) {
	var bindings []model.AccountModelRoute
	if s.db.Migrator().HasTable(&model.AccountModelRoute{}) {
		if err := s.db.WithContext(ctx).Where("account_id = ?", account.ID).Order("model_route_id asc").Find(&bindings).Error; err != nil {
			return nil, err
		}
	}
	routeRows := make([]map[string]any, 0, len(bindings))
	for _, binding := range bindings {
		var route model.ModelRoute
		if err := s.db.WithContext(ctx).First(&route, "id = ?", binding.ModelRouteID).Error; err != nil {
			continue
		}
		if !model.IsCanonicalRoute(route) {
			continue
		}
		routeRows = append(routeRows, map[string]any{
			"id": binding.ID, "route_id": route.ID, "model_id": route.LogicalModelID,
			"enabled": binding.Enabled && binding.Entitled,
		})
	}
	buckets, err := s.models.Quotas().ListByAccount(ctx, account.ID)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	quotaRows := make([]map[string]any, 0, len(buckets))
	for _, bucket := range buckets {
		quotaRows = append(quotaRows, map[string]any{
			"name": bucket.BucketKey, "remaining": bucket.Remaining, "total": bucket.Total,
			"reserved": bucket.Reserved, "reset_at": bucket.ResetAt, "unit": bucket.Unit,
			"refreshed_at": bucket.RefreshedAt,
		})
	}
	var activeJobs int64
	if s.db.Migrator().HasTable(&model.DispatchAttempt{}) {
		_ = s.db.WithContext(ctx).Model(&model.DispatchAttempt{}).
			Where("account_id = ? AND state IN ?", account.ID, []string{"created", "submitting", "accepted", "unknown"}).Count(&activeJobs).Error
	}
	health := "healthy"
	if account.Dead || account.Status == "disabled" {
		health = "unhealthy"
	} else if account.Status == "quota" || account.Status == "pending" || account.Fails > 0 {
		health = "degraded"
	}
	readiness, readinessDetail := dolaReadinessView(account)
	if account.Pool == "dola" && readiness != "ready" {
		health = "degraded"
	}
	sessionExpiresAt, sessionState := bytePlusSessionStatus(account, time.Now())
	if sessionState == "expired" {
		health = "unhealthy"
	} else if sessionState == "expiring" && health == "healthy" {
		health = "degraded"
	}
	label := strings.TrimSpace(account.AccountDisplayName)
	if label == "" {
		label = strings.TrimSpace(account.AccountEmail)
	}
	if label == "" {
		label = account.ID
	}
	return map[string]any{
		"id": account.ID, "provider": account.Pool, "label": label, "email": account.AccountEmail,
		"status": account.Status, "weight": account.Weight, "max_concurrency": account.Concurrency,
		"active_jobs": activeJobs, "health": health, "routes": routeRows, "quota_buckets": quotaRows,
		"success_total": account.SuccessTotal, "fail_total": account.FailTotal,
		"consecutive_failures": account.Fails, "upstream_failures": account.UpstreamFails,
		"image_limited": account.ImageLimited, "video_limited": account.VideoLimited,
		"last_used_at": account.LastUsedAt, "created_at": account.CreatedAt,
		"session_expires_at": sessionExpiresAt, "session_state": sessionState,
		"readiness": readiness, "readiness_detail": readinessDetail,
		"base_url": safeCustomMeta(account, "base_url"), "models": safeCustomMeta(account, "models"),
	}, nil
}

func bytePlusSessionStatus(account model.TokenAccount, now time.Time) (string, string) {
	if account.Pool != "byteplus" {
		return "", ""
	}
	expiry := bytePlusAccountSessionExpiry(account)
	if expiry.IsZero() {
		return "", "unknown"
	}
	state := "valid"
	if !expiry.After(now) {
		state = "expired"
	} else if !expiry.After(now.Add(6 * time.Hour)) {
		state = "expiring"
	}
	return expiry.UTC().Format(time.RFC3339), state
}

func safeCustomMeta(account model.TokenAccount, key string) string {
	if account.Pool != "custom" || account.Meta == nil || (key != "base_url" && key != "models") {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(account.Meta[key]))
}

func (s *AdminConsoleService) ImportAccount(ctx context.Context, input AccountImportInput) (map[string]any, error) {
	provider := normalizeAdminProvider(input.Provider)
	if provider == "" || isDeferredAccountProvider(provider) {
		return nil, errors.New("unsupported provider")
	}
	credential := credentialMap(input.Credential)
	secret := credentialSecret(input.Credential, credential)
	var account *model.TokenAccount
	var err error
	switch provider {
	case "chatgpt":
		account, err = s.tokenSvc.ImportChatGPTToken(ctx, secret, "")
	case "adobe":
		account, _, err = s.tokenSvc.ImportAdobeCookie(ctx, secret, adobeARPSessionToken(credential), "")
	case "byteplus":
		// BytePlus browser exports contain a large amount of untrusted profile,
		// tenant, storage and balance metadata. Only the actual Cookie header is
		// credential material; identity and quota are queried from BytePlus after
		// the import succeeds.
		secret = bytePlusCredentialSecret(input.Credential, credential)
		account, err = s.tokenSvc.ImportBytePlusCookie(ctx, secret)
		if err == nil && account != nil {
			err = s.tokenSvc.ConfigureBytePlusLogin(ctx, account.ID,
				firstMapString(credential, "email", "login_identity"),
				firstMapString(credential, "password", "login_secret"), secret)
		}
	case "runway":
		account, err = s.tokenSvc.ImportRunwayToken(ctx, secret, "")
	case "grok":
		account, err = s.tokenSvc.ImportGrokToken(ctx, secret, "")
	case "oreate":
		account, err = s.tokenSvc.ImportOreateAccount(ctx, secret, stringFromMap(credential, "email"), stringFromMap(credential, "ouid"), stringFromMap(credential, "user_agent"), int64FromMap(credential, "reg_ts"), stringFromMap(credential, "vip"), "")
	case "dola":
		account, err = s.tokenSvc.ImportDolaAccount(ctx, secret, stringFromMap(credential, "user_agent"))
	case "custom":
		models := canonicalModelsFromCredential(credential)
		if len(models) == 0 {
			return nil, errors.New("custom accounts must bind at least one existing canonical model")
		}
		account, err = s.tokenSvc.ImportCustomAccount(ctx, stringFromMap(credential, "base_url"), firstMapString(credential, "api_key", "key", "token"), strings.Join(models, ","), input.Label, intFromMap(credential, "weight"), intFromMap(credential, "max_concurrency"), strings.TrimSpace(input.ID))
	}
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, errors.New("provider did not return an account")
	}
	if label := strings.TrimSpace(input.Label); label != "" {
		if updated, updateErr := s.tokens.Update(ctx, account.Pool, account.ID, map[string]any{"account_display_name": label}); updateErr == nil {
			account = updated
		}
	}
	if err := s.bindAccountRoutes(ctx, *account, canonicalModelsFromCredential(credential)); err != nil {
		return nil, err
	}
	// Import is not accepted on faith. Resolve identity (where supported) and
	// refresh the provider balance before returning the account to the UI.
	_, _ = s.tokenSvc.Email(ctx, account.Pool, account.ID)
	_, quotaErr := s.RefreshAccountQuota(ctx, account.ID)
	if quotaErr != nil {
		return nil, quotaErr
	}
	account, err = s.tokens.Get(ctx, account.Pool, account.ID)
	if err != nil {
		return nil, err
	}
	return s.accountRow(ctx, *account)
}

func (s *AdminConsoleService) bindAccountRoutes(ctx context.Context, account model.TokenAccount, allowModels []string) error {
	allowed := make(map[string]bool, len(allowModels))
	for _, id := range allowModels {
		allowed[id] = true
	}
	if account.Pool == "custom" && s.db.Migrator().HasTable(&model.AccountModelRoute{}) {
		var existing []model.AccountModelRoute
		if err := s.db.WithContext(ctx).Where("account_id = ?", account.ID).Find(&existing).Error; err != nil {
			return err
		}
		for _, binding := range existing {
			var route model.ModelRoute
			if err := s.db.WithContext(ctx).First(&route, "id = ?", binding.ModelRouteID).Error; err != nil || route.Provider != "custom" || allowed[route.LogicalModelID] {
				continue
			}
			binding.Enabled = false
			if err := s.models.Routes().SetAccountRoute(ctx, &binding); err != nil {
				return err
			}
		}
	}
	for _, definition := range model.CanonicalRoutingCatalog() {
		if definition.Model.ID == model.DolaPublicVideoModel && account.Pool != "dola" {
			continue
		}
		if account.Pool == "custom" && !allowed[definition.Model.ID] {
			continue
		}
		for _, route := range definition.Routes {
			if route.Provider != account.Pool {
				continue
			}
			binding := &model.AccountModelRoute{
				AccountID: account.ID, ModelRouteID: route.ID, Enabled: true, Entitled: true,
				QuotaBucketKey: route.QuotaBucketKey,
			}
			if err := s.models.Routes().SetAccountRoute(ctx, binding); err != nil {
				return err
			}
		}
		if account.Pool == "custom" && allowed[definition.Model.ID] {
			routeID := "custom." + definition.Model.ID
			route := model.ModelRoute{
				ID: routeID, LogicalModelID: definition.Model.ID, Provider: "custom",
				RuntimeModel: definition.Model.ID, UpstreamModel: definition.Model.ID,
				Enabled: true, Priority: 10, Weight: 1, QuotaBucketKey: "custom.unmetered",
				QuotaCosts:   datatypes.JSON([]byte(`{"mode":"unmetered"}`)),
				Capabilities: aggregateCapabilities(definition.Routes),
			}
			if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "id"}},
				DoUpdates: clause.AssignmentColumns([]string{"enabled", "quota_costs", "capabilities", "updated_at"}),
			}).Create(&route).Error; err != nil {
				return err
			}
			if err := s.models.Routes().SetAccountRoute(ctx, &model.AccountModelRoute{
				AccountID: account.ID, ModelRouteID: routeID, Enabled: true, Entitled: true,
				QuotaBucketKey: route.QuotaBucketKey,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func aggregateCapabilities(routes []model.ModelRoute) datatypes.JSON {
	seen := make(map[string]json.RawMessage)
	for _, route := range routes {
		for _, profile := range model.DecodeCapabilityProfiles(route.Capabilities) {
			raw, _ := json.Marshal(profile)
			seen[string(raw)] = raw
		}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	profiles := make([]json.RawMessage, 0, len(keys))
	for _, key := range keys {
		profiles = append(profiles, seen[key])
	}
	raw, _ := json.Marshal(profiles)
	return datatypes.JSON(raw)
}

func (s *AdminConsoleService) UpdateAccount(ctx context.Context, accountID string, patch map[string]any) (map[string]any, error) {
	account, err := s.accountByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	updates := map[string]any{}
	if value, ok := patch["status"]; ok {
		status := strings.ToLower(strings.TrimSpace(fmt.Sprint(value)))
		if status != "active" && status != "disabled" {
			return nil, errors.New("status must be active or disabled")
		}
		updates["status"] = status
		// Manual disablement is an operator scheduling choice, not proof that the
		// credential is dead. Preserve the probe-derived dead flag when disabling;
		// an explicit re-enable is allowed to clear it.
		if status == "active" {
			updates["dead"] = false
		}
	}
	if value, ok := patch["weight"]; ok {
		weight := intValueSafe(value)
		if weight < -1000 || weight > 1000 {
			return nil, errors.New("weight must be between -1000 and 1000")
		}
		updates["weight"] = weight
	}
	if value, ok := patch["max_concurrency"]; ok {
		limit := intValueSafe(value)
		if limit < 0 || limit > 1000 {
			return nil, errors.New("max_concurrency must be between 0 and 1000")
		}
		updates["concurrency"] = limit
	}
	if account.Pool == "dola" {
		if status, ok := updates["status"]; ok {
			if err := s.tokens.SetDolaEnabled(ctx, account.ID, status == "active"); err != nil {
				return nil, err
			}
			delete(updates, "status")
			delete(updates, "dead")
			if status == "active" {
				go s.tokenSvc.verifyDolaReadiness(account.ID, true)
			}
			account, err = s.tokens.Get(ctx, "dola", account.ID)
			if err != nil {
				return nil, err
			}
		}
	}
	if len(updates) > 0 {
		account, err = s.tokens.Update(ctx, account.Pool, account.ID, updates)
		if err != nil {
			return nil, err
		}
	}
	return s.accountRow(ctx, *account)
}

func (s *AdminConsoleService) DeleteAccount(ctx context.Context, accountID string) error {
	account, err := s.accountByID(ctx, accountID)
	if err != nil {
		return err
	}
	// TokenService also removes a matching Adobe cookie refresh profile. Foreign
	// key cascades remove route bindings and quota buckets/reservations.
	return s.tokenSvc.Delete(ctx, account.Pool, account.ID)
}

func (s *AdminConsoleService) DeleteDeadAccounts(ctx context.Context) (int, error) {
	var accounts []model.TokenAccount
	if err := s.db.WithContext(ctx).
		Where("pool IN ? AND (dead = ? OR status = ?)", adminAccountProviders, true, "auth_error").
		Find(&accounts).Error; err != nil {
		return 0, err
	}
	ids := make([]string, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.ID)
	}
	// Delete in one statement so a database error cannot leave a half-deleted
	// batch. The schema cascades route bindings, quota buckets and reservations.
	return s.tokenSvc.DeleteBulk(ctx, ids)
}

func (s *AdminConsoleService) SetAccountRoute(ctx context.Context, accountID, bindingID string, enabled bool) error {
	account, err := s.accountByID(ctx, accountID)
	if err != nil {
		return err
	}
	var binding model.AccountModelRoute
	if err := s.db.WithContext(ctx).First(&binding, "id = ? AND account_id = ?", bindingID, account.ID).Error; err != nil {
		return err
	}
	binding.Enabled = enabled
	return s.models.Routes().SetAccountRoute(ctx, &binding)
}

func (s *AdminConsoleService) RefreshAccountQuota(ctx context.Context, accountID string) (map[string]any, error) {
	account, err := s.accountByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if account.Pool == "dola" {
		if account.Status == "disabled" {
			return nil, errors.New("请先启用账号，再验证 Dola 会话")
		}
		go s.tokenSvc.verifyDolaReadiness(account.ID, true)
		return map[string]any{"queued": true, "message": "已安排 Dola 协议会话验证，不消耗生成次数"}, nil
	}
	return s.tokenSvc.Quota(ctx, account.Pool, account.ID)
}

func trustedQuotaSnapshot(snapshot map[string]any) (remaining, total *float64, resetAt *time.Time, trusted bool) {
	if snapshot == nil || boolValueWithDefault(snapshot["unknown"], false) || boolValueWithDefault(snapshot["auth_failed"], false) {
		return nil, nil, nil, false
	}
	remainingValue, hasRemaining := numeric(snapshot["remaining"])
	if !hasRemaining || math.IsNaN(remainingValue) || math.IsInf(remainingValue, 0) || remainingValue < 0 {
		return nil, nil, nil, false
	}
	remaining = &remainingValue
	if totalValue, hasTotal := numeric(snapshot["total"]); hasTotal && !math.IsNaN(totalValue) && !math.IsInf(totalValue, 0) && totalValue >= 0 {
		total = &totalValue
	}
	resetAt = parseResetTime(snapshot["reset_after"])
	if resetAt == nil {
		resetAt = parseResetTime(snapshot["available_until"])
	}
	return remaining, total, resetAt, true
}

func (s *AdminConsoleService) accountByID(ctx context.Context, id string) (*model.TokenAccount, error) {
	var account model.TokenAccount
	if err := s.db.WithContext(ctx).First(&account, "id = ?", strings.TrimSpace(id)).Error; err != nil {
		return nil, err
	}
	return &account, nil
}

func (s *AdminConsoleService) ListLogs(ctx context.Context, filter LogListFilter) ([]map[string]any, int64, error) {
	query := s.db.WithContext(ctx).Model(&model.EventLog{})
	if filter.Artifacts {
		// Image rows reserve an object key before provider execution. A non-empty
		// file column is therefore not proof that an artifact exists; only terminal
		// successful events belong in the artifact gallery.
		query = query.Where("status = ? AND file <> ''", "success")
	}
	if value := strings.TrimSpace(filter.CredentialID); value != "" && s.db.Migrator().HasColumn(&model.EventLog{}, "api_credential_id") {
		query = query.Where("api_credential_id = ?", value)
	}
	if value := strings.TrimSpace(filter.Model); value != "" {
		query = query.Where("model = ?", value)
	}
	if value := strings.TrimSpace(filter.Kind); value != "" {
		query = query.Where("kind = ?", value)
	}
	if value := strings.TrimSpace(filter.Status); value != "" {
		query = query.Where("status = ?", value)
	}
	if value := strings.TrimSpace(filter.Source); value != "" {
		query = query.Where("source = ?", value)
	}
	if value := strings.TrimSpace(filter.Query); value != "" {
		like := "%" + value + "%"
		query = query.Where("request_id ILIKE ? OR prompt ILIKE ? OR model ILIKE ? OR error ILIKE ?", like, like, like, like)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	limit, offset := normalizePage(filter.Limit, filter.Offset)
	var events []model.EventLog
	if err := query.Order("ts desc").Limit(limit).Offset(offset).Find(&events).Error; err != nil {
		return nil, 0, err
	}
	credentialIDs := make([]string, 0)
	seenCredential := make(map[string]bool)
	for _, event := range events {
		if event.APICredentialID != "" && !seenCredential[event.APICredentialID] {
			seenCredential[event.APICredentialID] = true
			credentialIDs = append(credentialIDs, event.APICredentialID)
		}
	}
	credentials := make(map[string]model.APICredential, len(credentialIDs))
	if len(credentialIDs) > 0 {
		var items []model.APICredential
		if err := s.db.WithContext(ctx).Where("id IN ?", credentialIDs).Find(&items).Error; err != nil {
			return nil, 0, err
		}
		for _, item := range items {
			credentials[item.ID] = item
		}
	}
	rows := make([]map[string]any, 0, len(events))
	for _, event := range events {
		credential := map[string]any(nil)
		if item, ok := credentials[event.APICredentialID]; ok {
			credential = map[string]any{"id": item.ID, "name": item.Name, "key_preview": item.KeyPreview}
		}
		row := map[string]any{
			"id": event.ID, "request_id": event.RequestID, "model": event.Model, "kind": event.Kind,
			"status": event.Status, "provider": event.Provider, "account_id": event.AccountID, "account_label": event.AccountEmail,
			"duration_ms": event.ElapsedMS, "error": safeStoredGenerationError(event.Error), "created_at": event.TS,
			"api_credential_id": event.APICredentialID, "credential": credential,
			"prompt": event.Prompt, "ratio": event.Ratio, "resolution": event.Resolution,
			"duration": event.Duration, "refs": event.Refs, "deai": event.DeAI, "source": event.Source,
			"cost": event.Cost, "mime_type": eventMimeType(event),
		}
		if event.Status == "success" && strings.TrimSpace(event.File) != "" && (event.Kind == "image" || event.Kind == "video") {
			contentURL := "/admin/api/artifacts/" + event.ID + "/content"
			row["content_url"] = contentURL
			row["thumbnail_url"] = contentURL
		}
		if filter.Artifacts {
			row["size_bytes"] = nil
		}
		rows = append(rows, row)
	}
	return rows, total, nil
}

func (s *AdminConsoleService) GetSettings(ctx context.Context) (map[string]any, error) {
	values := map[string]any{
		"logs_retention_days": 30, "artifacts_retention_days": 30,
		"outbound_proxy": "", "public_base_url": s.cfg.PublicBaseURL,
	}
	for settingKey, responseKey := range map[string]string{
		"logs.retention_days": "logs_retention_days", "artifacts.retention_days": "artifacts_retention_days",
		"proxy.url": "outbound_proxy", "public.base_url": "public_base_url",
	} {
		value, err := s.settings.GetValue(ctx, settingKey)
		if err != nil {
			return nil, err
		}
		if value == "" {
			continue
		}
		if strings.HasSuffix(responseKey, "_days") {
			if parsed, parseErr := strconv.Atoi(value); parseErr == nil {
				values[responseKey] = parsed
			}
		} else {
			values[responseKey] = value
		}
	}
	providers := map[string]bool{}
	for _, pool := range SchedulableProviders() {
		enabled, err := providerEnabled(ctx, s.settings, pool)
		if err != nil {
			return nil, err
		}
		providers[pool] = enabled
	}
	values["providers_enabled"] = providers
	return values, nil
}

func (s *AdminConsoleService) UpdateSettings(ctx context.Context, input map[string]any) (map[string]any, error) {
	updates := map[string]string{}
	for responseKey, settingKey := range map[string]string{
		"logs_retention_days": "logs.retention_days", "artifacts_retention_days": "artifacts.retention_days",
	} {
		if value, ok := input[responseKey]; ok {
			days := intValueSafe(value)
			if days < 1 || days > 3650 {
				return nil, fmt.Errorf("%s must be between 1 and 3650", responseKey)
			}
			updates[settingKey] = strconv.Itoa(days)
		}
	}
	if value, ok := input["outbound_proxy"]; ok {
		proxy, err := normalizeOutboundProxy(fmt.Sprint(value))
		if err != nil {
			return nil, err
		}
		updates["proxy.url"] = proxy
	}
	if value, ok := input["public_base_url"]; ok {
		production := s.cfg != nil && strings.EqualFold(strings.TrimSpace(s.cfg.AppEnv), "production")
		baseURL, err := normalizeAdminPublicOrigin(fmt.Sprint(value), production)
		if err != nil {
			return nil, err
		}
		updates["public.base_url"] = baseURL
	}
	if raw, ok := input["providers_enabled"]; ok {
		switches, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("providers_enabled must be an object of pool => boolean")
		}
		for pool, flag := range switches {
			normalized := normalizePool(pool)
			if normalized == "" {
				return nil, fmt.Errorf("providers_enabled contains unknown provider %q", pool)
			}
			enabled, ok := flag.(bool)
			if !ok {
				return nil, fmt.Errorf("providers_enabled.%s must be a boolean", pool)
			}
			updates[providerSettingKey(normalized)] = strconv.FormatBool(enabled)
		}
	}
	if err := s.settings.UpsertValues(ctx, updates); err != nil {
		return nil, err
	}
	return s.GetSettings(ctx)
}

func normalizeOutboundProxy(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return "", errors.New("outbound_proxy must be an absolute http(s) or socks5 URL")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", errors.New("outbound_proxy must be an absolute http(s) or socks5 URL")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("outbound_proxy must not contain a query or fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	return parsed.String(), nil
}

func normalizeAdminPublicOrigin(raw string, production bool) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("public_base_url must be an absolute HTTP(S) origin")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("public_base_url must be an absolute HTTP(S) origin")
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("public_base_url must not contain a path, query, user info, or fragment")
	}
	if production && (parsed.Scheme != "https" || localOriginHost(parsed.Hostname())) {
		return "", errors.New("public_base_url must be a non-local HTTPS origin in production")
	}
	parsed.Path, parsed.RawPath = "", ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func localOriginHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return false
}

func normalizeAdminProvider(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "openai", "chatgpt":
		return "chatgpt"
	case "adobe", "byteplus", "runway", "grok", "oreate", "dola", "custom":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func normalizePage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func credentialMap(value any) map[string]any {
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	if raw, ok := value.(json.RawMessage); ok {
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return out
	}
	if text, ok := value.(string); ok && strings.HasPrefix(strings.TrimSpace(text), "{") {
		var out map[string]any
		_ = json.Unmarshal([]byte(text), &out)
		return out
	}
	return map[string]any{}
}

func credentialSecret(value any, values map[string]any) string {
	if text, ok := value.(string); ok {
		text = strings.TrimSpace(text)
		// The admin UI deliberately accepts both a raw token/Cookie header and a
		// complete browser-export JSON document.  When the latter arrives as a
		// JSON string, credentialMap has already decoded it into values; never
		// persist or send the whole JSON blob as if it were a Cookie header.
		if len(values) > 0 && (strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[")) {
			if cookie := cookieFromCredential(values); cookie != "" {
				return cookie
			}
			if secret := firstMapString(values, "access_token", "sso_token", "token", "api_key", "key"); secret != "" {
				return secret
			}
		}
		return text
	}
	if cookie := cookieFromCredential(values); cookie != "" {
		return cookie
	}
	return firstMapString(values, "access_token", "sso_token", "token", "api_key", "key")
}

func bytePlusCredentialSecret(value any, values map[string]any) string {
	if text, ok := value.(string); ok {
		text = strings.TrimSpace(text)
		if len(values) == 0 {
			return strings.TrimSpace(strings.TrimPrefix(text, "Cookie:"))
		}
	}
	if text := firstMapString(values, "cookie_string", "cookie_header"); text != "" {
		return strings.TrimSpace(strings.TrimPrefix(text, "Cookie:"))
	}
	raw, ok := values["cookies"].([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(raw))
	for _, entry := range raw {
		cookie, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		name, cookieValue := stringFromMap(cookie, "name"), stringFromMap(cookie, "value")
		if name != "" && cookieValue != "" {
			parts = append(parts, name+"="+cookieValue)
		}
	}
	return strings.Join(parts, "; ")
}

func adobeARPSessionToken(values map[string]any) string {
	aliases := map[string]struct{}{
		"x-arp-session-id":  {},
		"x_arp_session_id":  {},
		"arp_session_id":    {},
		"arp_session_token": {},
		"arpsessiontoken":   {},
	}
	find := func(source map[string]any) string {
		for key, raw := range source {
			if _, ok := aliases[strings.ToLower(strings.TrimSpace(key))]; !ok || raw == nil {
				continue
			}
			if token := strings.TrimSpace(fmt.Sprint(raw)); token != "" && token != "<nil>" {
				return token
			}
		}
		return ""
	}
	if token := find(values); token != "" {
		return token
	}
	if headers, ok := values["headers"].(map[string]any); ok {
		return find(headers)
	}
	return ""
}

func cookieFromCredential(values map[string]any) string {
	if text := firstMapString(values, "cookie", "cookie_string", "cookie_header"); text != "" {
		return strings.TrimSpace(strings.TrimPrefix(text, "Cookie:"))
	}
	raw, ok := values["cookies"].([]any)
	if !ok {
		// Some browser exporters provide a name/value object instead of the
		// conventional cookies[] array. Sort the keys so the normalized Cookie
		// header and its account fingerprint stay stable across imports.
		if cookieMap, mapOK := values["cookies_map"].(map[string]any); mapOK {
			keys := make([]string, 0, len(cookieMap))
			for name, value := range cookieMap {
				if strings.TrimSpace(name) != "" && strings.TrimSpace(fmt.Sprint(value)) != "" {
					keys = append(keys, name)
				}
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, name := range keys {
				parts = append(parts, name+"="+strings.TrimSpace(fmt.Sprint(cookieMap[name])))
			}
			return strings.Join(parts, "; ")
		}
		return ""
	}
	parts := make([]string, 0, len(raw))
	for _, entry := range raw {
		cookie, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		name, value := stringFromMap(cookie, "name"), stringFromMap(cookie, "value")
		if name != "" && value != "" {
			parts = append(parts, name+"="+value)
		}
	}
	return strings.Join(parts, "; ")
}

func canonicalModelsFromCredential(values map[string]any) []string {
	seen := map[string]bool{}
	var candidates []string
	switch raw := values["models"].(type) {
	case string:
		candidates = strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == ';' })
	case []any:
		for _, value := range raw {
			candidates = append(candidates, fmt.Sprint(value))
		}
	case map[string]any:
		for key := range raw {
			candidates = append(candidates, key)
		}
	}
	for _, candidate := range candidates {
		id := strings.TrimSpace(candidate)
		if model.IsCanonicalModelID(id) {
			seen[id] = true
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func firstMapString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringFromMap(values, key); value != "" {
			return value
		}
	}
	return ""
}

func stringFromMap(values map[string]any, key string) string {
	if values == nil || values[key] == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(values[key]))
}

func intFromMap(values map[string]any, key string) int { return intValueSafe(values[key]) }

func int64FromMap(values map[string]any, key string) int64 {
	value, _ := strconv.ParseInt(strings.TrimSpace(fmt.Sprint(values[key])), 10, 64)
	return value
}

func intValueSafe(value any) int {
	number, ok := numeric(value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0
	}
	return int(number)
}

func numeric(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		n, err := typed.Float64()
		return n, err == nil
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func parseResetTime(value any) *time.Time {
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "" || text == "<nil>" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, text); err == nil {
			return &parsed
		}
	}
	return nil
}

func mimeForKind(kind string) string {
	if kind == "video" {
		return "video/mp4"
	}
	return "image/png"
}

func eventMimeType(event model.EventLog) string {
	if value := strings.ToLower(strings.TrimSpace(event.MimeType)); value != "" {
		return value
	}
	return mimeForKind(event.Kind)
}
