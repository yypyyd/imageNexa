package repo

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"backend/internal/model"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ModelRouteRepository struct {
	db *gorm.DB
}

func NewModelRouteRepository(db *gorm.DB) *ModelRouteRepository {
	return &ModelRouteRepository{db: db}
}

func canonicalDefinitions() []model.CanonicalModelDefinition {
	return model.CanonicalRoutingCatalog()
}

func fallbackLogicalModels() []model.LogicalModel {
	definitions := canonicalDefinitions()
	out := make([]model.LogicalModel, 0, len(definitions))
	for _, definition := range definitions {
		out = append(out, definition.Model)
	}
	return out
}

func canonicalLogicalIDs() []string {
	definitions := canonicalDefinitions()
	ids := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		ids = append(ids, definition.Model.ID)
	}
	return ids
}

func canonicalRouteIDs() []string {
	definitions := canonicalDefinitions()
	var ids []string
	for _, definition := range definitions {
		for _, route := range definition.Routes {
			ids = append(ids, route.ID)
		}
	}
	return ids
}

func fallbackRoutes(logicalID string) []model.ModelRoute {
	for _, definition := range canonicalDefinitions() {
		if definition.Model.ID == logicalID {
			out := append([]model.ModelRoute(nil), definition.Routes...)
			sortRoutes(out)
			return out
		}
	}
	return nil
}

func sortRoutes(routes []model.ModelRoute) {
	sort.SliceStable(routes, func(i, j int) bool {
		if routes[i].Priority != routes[j].Priority {
			return routes[i].Priority > routes[j].Priority
		}
		if routes[i].Weight != routes[j].Weight {
			return routes[i].Weight > routes[j].Weight
		}
		return routes[i].ID < routes[j].ID
	})
}

func (r *ModelRouteRepository) hasRoutingTables() bool {
	return r != nil && r.db != nil && r.db.Migrator().HasTable(&model.LogicalModel{}) && r.db.Migrator().HasTable(&model.ModelRoute{})
}

func (r *ModelRouteRepository) ListLogical(ctx context.Context, enabledOnly bool) ([]model.LogicalModel, error) {
	if !r.hasRoutingTables() {
		items := fallbackLogicalModels()
		if enabledOnly {
			filtered := items[:0]
			for _, item := range items {
				if item.Enabled {
					filtered = append(filtered, item)
				}
			}
			items = filtered
		}
		return items, nil
	}
	var items []model.LogicalModel
	// The database can contain historical rows referenced by immutable dispatch
	// history. They are tombstones, not part of the closed 2API catalog.
	query := r.db.WithContext(ctx).Where("id IN ?", canonicalLogicalIDs()).Order("weight desc, created_at asc, id asc")
	if enabledOnly {
		query = query.Where("enabled = ?", true)
	}
	if err := query.Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

func (r *ModelRouteRepository) GetLogical(ctx context.Context, logicalID string) (*model.LogicalModel, error) {
	logicalID = strings.TrimSpace(logicalID)
	if !model.IsCanonicalModelID(logicalID) {
		return nil, gorm.ErrRecordNotFound
	}
	if !r.hasRoutingTables() {
		for _, item := range fallbackLogicalModels() {
			if item.ID == logicalID {
				copy := item
				return &copy, nil
			}
		}
		return nil, gorm.ErrRecordNotFound
	}
	var item model.LogicalModel
	if err := r.db.WithContext(ctx).First(&item, "id = ?", logicalID).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

func (r *ModelRouteRepository) ListRoutes(ctx context.Context, logicalID string, enabledOnly bool) ([]model.ModelRoute, error) {
	logicalID = strings.TrimSpace(logicalID)
	if !model.IsCanonicalModelID(logicalID) {
		return nil, nil
	}
	if !r.hasRoutingTables() {
		items := fallbackRoutes(logicalID)
		if enabledOnly {
			filtered := items[:0]
			for _, item := range items {
				if item.Enabled {
					filtered = append(filtered, item)
				}
			}
			items = filtered
		}
		return items, nil
	}
	var items []model.ModelRoute
	query := r.db.WithContext(ctx).Where("logical_model_id = ?", logicalID).Order("priority desc, weight desc, id asc")
	query = query.Where("id IN ? OR (provider = ? AND id = ?)", canonicalRouteIDs(), "custom", "custom."+logicalID)
	if enabledOnly {
		query = query.Where("enabled = ?", true)
	}
	if err := query.Find(&items).Error; err != nil {
		return nil, err
	}
	closed := items[:0]
	for _, item := range items {
		if model.IsCanonicalRoute(item) {
			closed = append(closed, item)
		}
	}
	return closed, nil
}

func (r *ModelRouteRepository) MatchRoutes(ctx context.Context, logicalID string, req model.RouteRequirements) ([]model.ModelRoute, error) {
	routes, err := r.ListRoutes(ctx, logicalID, true)
	if err != nil {
		return nil, err
	}
	out := make([]model.ModelRoute, 0, len(routes))
	for _, route := range routes {
		if route.Supports(req) {
			out = append(out, route)
		}
	}
	sortRoutes(out)
	return out, nil
}

func (r *ModelRouteRepository) AccountAllowsRoute(ctx context.Context, accountID, routeID string) (bool, error) {
	return r.accountAllowsRoute(ctx, accountID, routeID, true)
}

// AccountAllowsRouteInFlight revalidates the durable authorization boundary
// for an account that was already selected for the current request. A
// temporary failure may put the binding on cooldown between the first attempt
// and its bounded in-request retry; that cooldown is admission control for new
// requests, not a revocation of work already admitted. Explicit disablement or
// loss of entitlement still stops the in-flight attempt immediately.
func (r *ModelRouteRepository) AccountAllowsRouteInFlight(ctx context.Context, accountID, routeID string) (bool, error) {
	return r.accountAllowsRoute(ctx, accountID, routeID, false)
}

func (r *ModelRouteRepository) accountAllowsRoute(ctx context.Context, accountID, routeID string, checkCooldown bool) (bool, error) {
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(routeID) == "" {
		return false, nil
	}
	if r == nil || r.db == nil || !r.db.Migrator().HasTable(&model.AccountModelRoute{}) {
		return true, nil // compatibility until account entitlements are imported
	}
	var binding model.AccountModelRoute
	err := r.db.WithContext(ctx).First(&binding, "account_id = ? AND model_route_id = ?", accountID, routeID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// Once the routing schema exists, bindings are the authorization boundary.
		// Falling back to provider-wide access here would silently let an account
		// serve models it was never probed or entitled for.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return accountRouteBindingAllows(binding, time.Now(), checkCooldown), nil
}

func accountRouteBindingAllows(binding model.AccountModelRoute, now time.Time, checkCooldown bool) bool {
	if !binding.Enabled || !binding.Entitled {
		return false
	}
	return !checkCooldown || binding.CooldownUntil == nil || !binding.CooldownUntil.After(now)
}

func (r *ModelRouteRepository) AccountRoute(ctx context.Context, accountID, routeID string) (*model.AccountModelRoute, error) {
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(routeID) == "" {
		return nil, gorm.ErrRecordNotFound
	}
	if r == nil || r.db == nil || !r.db.Migrator().HasTable(&model.AccountModelRoute{}) {
		return nil, gorm.ErrRecordNotFound
	}
	var binding model.AccountModelRoute
	if err := r.db.WithContext(ctx).First(&binding, "account_id = ? AND model_route_id = ?", accountID, routeID).Error; err != nil {
		return nil, err
	}
	return &binding, nil
}

// AccountRoutesForRoute loads all requested account bindings in one query. The
// returned map intentionally omits missing bindings: once routing tables exist,
// an absent account+route row is not an implicit entitlement.
func (r *ModelRouteRepository) AccountRoutesForRoute(ctx context.Context, accountIDs []string, routeID string) (map[string]model.AccountModelRoute, error) {
	out := make(map[string]model.AccountModelRoute, len(accountIDs))
	if len(accountIDs) == 0 || strings.TrimSpace(routeID) == "" {
		return out, nil
	}
	if r == nil || r.db == nil || !r.db.Migrator().HasTable(&model.AccountModelRoute{}) {
		return out, gorm.ErrRecordNotFound
	}
	var bindings []model.AccountModelRoute
	if err := r.db.WithContext(ctx).
		Where("model_route_id = ? AND account_id IN ?", strings.TrimSpace(routeID), accountIDs).
		Find(&bindings).Error; err != nil {
		return nil, err
	}
	for _, binding := range bindings {
		out[binding.AccountID] = binding
	}
	return out, nil
}

func (r *ModelRouteRepository) GetRoute(ctx context.Context, routeID string) (*model.ModelRoute, error) {
	if r == nil || r.db == nil || strings.TrimSpace(routeID) == "" || !r.db.Migrator().HasTable(&model.ModelRoute{}) {
		return nil, gorm.ErrRecordNotFound
	}
	var route model.ModelRoute
	if err := r.db.WithContext(ctx).First(&route, "id = ?", strings.TrimSpace(routeID)).Error; err != nil {
		return nil, err
	}
	if !model.IsCanonicalRoute(route) {
		return nil, gorm.ErrRecordNotFound
	}
	return &route, nil
}

func (r *ModelRouteRepository) SetAccountRoute(ctx context.Context, binding *model.AccountModelRoute) error {
	if binding == nil || strings.TrimSpace(binding.AccountID) == "" || strings.TrimSpace(binding.ModelRouteID) == "" {
		return errors.New("account and model route are required")
	}
	if binding.ID == "" {
		binding.ID = binding.AccountID + ":" + binding.ModelRouteID
	}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "account_id"}, {Name: "model_route_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"enabled", "entitled", "quota_bucket_key", "cooldown_until", "last_failure_class", "last_failure_at", "updated_at"}),
	}).Create(binding).Error
}

func (r *ModelRouteRepository) RecordAccountRouteResult(ctx context.Context, accountID, routeID, failureClass string, success bool) error {
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(routeID) == "" || r == nil || r.db == nil || !r.db.Migrator().HasTable(&model.AccountModelRoute{}) {
		return nil
	}
	now := time.Now()
	if success {
		return r.db.WithContext(ctx).Model(&model.AccountModelRoute{}).
			Where("account_id = ? AND model_route_id = ?", accountID, routeID).
			Updates(map[string]any{"consecutive_fails": 0, "cooldown_until": nil, "success_total": gorm.Expr("success_total + 1"), "last_success_at": &now, "updated_at": now}).Error
	}
	patch := map[string]any{"consecutive_fails": gorm.Expr("consecutive_fails + 1"), "last_failure_class": failureClass, "last_failure_at": &now, "updated_at": now}
	if failureClass == "entitlement" {
		patch["entitled"] = false
	}
	if failureClass == "temporary" {
		cooldown := now.Add(45 * time.Second)
		patch["cooldown_until"] = &cooldown
	}
	return r.db.WithContext(ctx).Model(&model.AccountModelRoute{}).
		Where("account_id = ? AND model_route_id = ?", accountID, routeID).Updates(patch).Error
}

func (r *ModelRouteRepository) UpdateRoute(ctx context.Context, routeID string, patch map[string]any) (*model.ModelRoute, error) {
	delete(patch, "id")
	delete(patch, "logical_model_id")
	delete(patch, "provider")
	delete(patch, "runtime_model")
	patch["updated_at"] = time.Now()
	if err := r.db.WithContext(ctx).Model(&model.ModelRoute{}).Where("id = ?", routeID).Updates(patch).Error; err != nil {
		return nil, err
	}
	var item model.ModelRoute
	if err := r.db.WithContext(ctx).First(&item, "id = ?", routeID).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

func routeModelConfig(logical model.LogicalModel, route model.ModelRoute) model.ModelConfig {
	item := model.ModelConfig{ID: route.RuntimeModel, Alias: logical.ID, Name: logical.Name, Type: logical.Kind,
		Provider: route.Provider, Enabled: logical.Enabled && route.Enabled, UpstreamModel: route.UpstreamModel,
		Weight: logical.Weight, GenerationCount: logical.GenerationCount, CreatedAt: logical.CreatedAt, UpdatedAt: logical.UpdatedAt}
	profiles := model.DecodeCapabilityProfiles(route.Capabilities)
	if len(profiles) == 0 {
		return item
	}
	profile := profiles[0]
	item.Ratios = jsonStrings(profile.Ratios)
	item.Resolutions = jsonStrings(profile.Resolutions)
	item.Durations = jsonStrings(profile.Durations)
	item.ImageToImage = containsFold(profile.Operations, "edit")
	item.MaxReferenceImages = profile.MaxReferenceImages
	item.MaxReferenceVideos = profile.MaxReferenceVideos
	item.MaxReferenceAudios = profile.MaxReferenceAudios
	item.MaxReferenceMedia = profile.MaxReferenceMedia
	item.SupportsAudioOutput = profile.SupportsAudioOutput
	item.ReferenceMode = profile.ReferenceMode
	return item
}

func (r *ModelRouteRepository) RouteConfig(ctx context.Context, route model.ModelRoute) (*model.ModelConfig, error) {
	logical, err := r.GetLogical(ctx, route.LogicalModelID)
	if err != nil {
		return nil, err
	}
	item := routeModelConfig(*logical, route)
	return &item, nil
}

func aggregateModelConfig(logical model.LogicalModel, routes []model.ModelRoute) model.ModelConfig {
	item := model.ModelConfig{ID: logical.ID, Name: logical.Name, Type: logical.Kind, Provider: "2api", Enabled: logical.Enabled,
		Weight: logical.Weight, GenerationCount: logical.GenerationCount, CreatedAt: logical.CreatedAt, UpdatedAt: logical.UpdatedAt}
	var ratios, resolutions, durations []string
	for _, route := range routes {
		if !route.Enabled {
			continue
		}
		for _, profile := range model.DecodeCapabilityProfiles(route.Capabilities) {
			ratios = unionFold(ratios, profile.Ratios)
			resolutions = unionFold(resolutions, profile.Resolutions)
			durations = unionFold(durations, profile.Durations)
			item.ImageToImage = item.ImageToImage || containsFold(profile.Operations, "edit")
			item.MaxReferenceImages = max(item.MaxReferenceImages, profile.MaxReferenceImages)
			item.MaxReferenceVideos = max(item.MaxReferenceVideos, profile.MaxReferenceVideos)
			item.MaxReferenceAudios = max(item.MaxReferenceAudios, profile.MaxReferenceAudios)
			item.MaxReferenceMedia = max(item.MaxReferenceMedia, profile.MaxReferenceMedia)
			item.SupportsAudioOutput = item.SupportsAudioOutput || profile.SupportsAudioOutput
			if item.ReferenceMode == "" {
				item.ReferenceMode = profile.ReferenceMode
			} else if item.ReferenceMode != profile.ReferenceMode {
				item.ReferenceMode = "mixed"
			}
		}
	}
	item.Ratios, item.Resolutions, item.Durations = jsonStrings(ratios), jsonStrings(resolutions), jsonStrings(durations)
	if item.ReferenceMode == "" {
		item.ReferenceMode = "none"
	}
	return item
}

func jsonStrings(values []string) datatypes.JSON {
	raw, _ := json.Marshal(values)
	return datatypes.JSON(raw)
}

func unionFold(dst, src []string) []string {
	for _, candidate := range src {
		if !containsFold(dst, candidate) {
			dst = append(dst, candidate)
		}
	}
	return dst
}

func containsFold(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(wanted)) {
			return true
		}
	}
	return false
}
