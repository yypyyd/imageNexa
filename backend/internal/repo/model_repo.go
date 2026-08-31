package repo

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"backend/internal/model"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type ModelRepository struct {
	db       *gorm.DB
	routes   *ModelRouteRepository
	quotas   *QuotaRepository
	dispatch *DispatchRepository
}

func NewModelRepository(db *gorm.DB) *ModelRepository {
	return &ModelRepository{db: db, routes: NewModelRouteRepository(db), quotas: NewQuotaRepository(db), dispatch: NewDispatchRepository(db)}
}

func (r *ModelRepository) Routes() *ModelRouteRepository { return r.routes }
func (r *ModelRepository) Quotas() *QuotaRepository      { return r.quotas }
func (r *ModelRepository) Dispatch() *DispatchRepository { return r.dispatch }

// IncrementGenerationCount bumps a model's persistent success counter by 1.
// Best-effort: a missing model id is a no-op (0 rows affected, no error).
func (r *ModelRepository) IncrementGenerationCount(ctx context.Context, modelID string) error {
	if !model.IsCanonicalModelID(modelID) {
		// Internal runtime ids are never counters. Resolve a matching canonical id
		// only for legacy callers that still pass a route config.
		for _, definition := range model.CanonicalRoutingCatalog() {
			for _, route := range definition.Routes {
				if route.RuntimeModel == modelID {
					modelID = definition.Model.ID
					break
				}
			}
		}
	}
	if r.routes.hasRoutingTables() {
		return r.db.WithContext(ctx).Model(&model.LogicalModel{}).Where("id = ?", modelID).
			UpdateColumn("generation_count", gorm.Expr("generation_count + 1")).Error
	}
	return nil
}

func (r *ModelRepository) List(ctx context.Context) ([]model.ModelConfig, error) {
	logicalModels, err := r.routes.ListLogical(ctx, false)
	if err != nil {
		return nil, err
	}
	items := make([]model.ModelConfig, 0, len(logicalModels))
	for _, logical := range logicalModels {
		routes, routeErr := r.routes.ListRoutes(ctx, logical.ID, false)
		if routeErr != nil {
			return nil, routeErr
		}
		items = append(items, aggregateModelConfig(logical, routes))
	}
	return items, nil
}

func (r *ModelRepository) Get(ctx context.Context, modelID string) (*model.ModelConfig, error) {
	logical, err := r.routes.GetLogical(ctx, strings.TrimSpace(modelID))
	if err != nil {
		return nil, err
	}
	routes, err := r.routes.ListRoutes(ctx, logical.ID, false)
	if err != nil {
		return nil, err
	}
	item := aggregateModelConfig(*logical, routes)
	return &item, nil
}

func (r *ModelRepository) NameMap(ctx context.Context) (map[string]string, error) {
	items, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(items))
	for _, item := range items {
		out[item.ID] = item.EffectiveName()
	}
	return out, nil
}

func JSONStrings(v datatypes.JSON) []string {
	if len(v) == 0 {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(v), &out); err == nil {
		return out
	}
	return []string{}
}

// CanonicalRatio normalizes the two ratio spellings accepted by generation
// inputs to the single W:H spelling exposed by every model-discovery endpoint.
// Only numeric ratios are rewritten, so unrelated values are left untouched.
func CanonicalRatio(value string) string {
	value = strings.TrimSpace(value)
	left, right, ok := strings.Cut(strings.ToLower(value), "x")
	if !ok || strings.Contains(right, "x") {
		return value
	}
	if _, err := strconv.Atoi(strings.TrimSpace(left)); err != nil {
		return value
	}
	if _, err := strconv.Atoi(strings.TrimSpace(right)); err != nil {
		return value
	}
	return strings.TrimSpace(left) + ":" + strings.TrimSpace(right)
}

func CanonicalRatios(values []string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = CanonicalRatio(value)
	}
	return out
}

func JSONRatios(v datatypes.JSON) []string {
	return CanonicalRatios(JSONStrings(v))
}

func (r *ModelRepository) Create(ctx context.Context, item *model.ModelConfig) error {
	if item == nil || !model.IsCanonicalModelID(item.ID) {
		return errors.New("the public model catalog is closed; bind a route to an existing canonical model")
	}
	return r.db.WithContext(ctx).Model(&model.LogicalModel{}).Where("id = ?", item.ID).
		Updates(map[string]any{"enabled": item.Enabled, "name": item.Name, "weight": item.Weight, "updated_at": time.Now()}).Error
}

func (r *ModelRepository) Update(ctx context.Context, modelID string, patch map[string]any) (*model.ModelConfig, error) {
	if !model.IsCanonicalModelID(modelID) {
		return nil, gorm.ErrRecordNotFound
	}
	allowed := map[string]any{"updated_at": time.Now()}
	for _, key := range []string{"name", "enabled", "weight"} {
		if value, ok := patch[key]; ok {
			allowed[key] = value
		}
	}
	if err := r.db.WithContext(ctx).Model(&model.LogicalModel{}).Where("id = ?", modelID).Updates(allowed).Error; err != nil {
		return nil, err
	}
	return r.Get(ctx, modelID)
}

func (r *ModelRepository) Delete(ctx context.Context, modelID string) (int64, error) {
	if !model.IsCanonicalModelID(modelID) {
		return 0, nil
	}
	res := r.db.WithContext(ctx).Model(&model.LogicalModel{}).Where("id = ?", modelID).
		Updates(map[string]any{"enabled": false, "updated_at": time.Now()})
	return res.RowsAffected, res.Error
}
