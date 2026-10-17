package service

import (
	"context"

	"backend/internal/model"
)

// Totals describe configured/enabled resources, not a promise of instantaneous
// dispatch capacity. Count in SQL so account pagination cannot truncate them.
func (s *AdminConsoleService) overviewTotals(ctx context.Context, health map[string]map[string]int64) (map[string]int64, error) {
	ids := make([]string, 0)
	for _, definition := range model.CanonicalRoutingCatalog() {
		ids = append(ids, definition.Model.ID)
	}
	totals := map[string]int64{"models": 0, "routes": 0, "accounts": 0, "keys": 0}
	var count int64
	if err := s.db.WithContext(ctx).Model(&model.LogicalModel{}).
		Where("id IN ? AND enabled = ?", ids, true).Count(&count).Error; err != nil {
		return nil, err
	}
	totals["models"] = count
	if err := s.db.WithContext(ctx).Model(&model.ModelRoute{}).
		Joins("JOIN logical_models ON logical_models.id = model_routes.logical_model_id").
		Where("logical_models.id IN ? AND logical_models.enabled = ? AND model_routes.enabled = ? AND model_routes.provider IN ?", ids, true, true, adminAccountProviders).
		Count(&count).Error; err != nil {
		return nil, err
	}
	totals["routes"] = count
	for _, provider := range adminAccountProviders {
		totals["accounts"] += health[provider]["active"]
	}
	if err := s.db.WithContext(ctx).Model(&model.APICredential{}).
		Where("status = ? AND revoked_at IS NULL", model.APICredentialStatusActive).Count(&count).Error; err != nil {
		return nil, err
	}
	totals["keys"] = count
	return totals, nil
}
