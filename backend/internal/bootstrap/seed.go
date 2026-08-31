package bootstrap

import (
	"context"

	"backend/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// seedDefaults contains only control-plane settings. Canonical models/routes
// are versioned data migrations; startup never recreates legacy aliases.
func seedDefaults(ctx context.Context, db *gorm.DB) error {
	defaults := []model.SiteSetting{
		{Key: "site.title", Value: "2API"},
		{Key: "proxy.url", Value: ""},
		{Key: "logs.retention_days", Value: "30"},
		{Key: "artifacts.retention_days", Value: "30"},
	}
	for _, item := range defaults {
		if err := db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&item).Error; err != nil {
			return err
		}
	}
	return nil
}
