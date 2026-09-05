package repo

import (
	"context"
	"time"

	"backend/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// UpsertBytePlusLogin stores the password-login material used only by 2API's
// internal renewal worker. It never exposes these fields through admin JSON.
func (r *RefreshProfileRepository) UpsertBytePlusLogin(ctx context.Context, accountID, identity, secret string, nextRetryAt time.Time) error {
	now := time.Now()
	item := model.RefreshProfile{
		ID: accountID, Name: "Lumina " + identity, Pool: "byteplus", Kind: "byteplus_password",
		LoginIdentity: identity, LoginSecret: secret, Enabled: true, IntervalSeconds: 3600,
		NextRetryAt: &nextRetryAt, CreatedAt: now, UpdatedAt: now,
	}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"name": identity, "pool": "byteplus", "kind": "byteplus_password",
			"login_identity": identity, "login_secret": secret, "enabled": true,
			"interval_seconds": 3600, "next_retry_at": nextRetryAt, "last_error": "",
			"consecutive_failures": 0, "updated_at": now,
		}),
	}).Create(&item).Error
}

type RefreshProfileRepository struct {
	db *gorm.DB
}

func NewRefreshProfileRepository(db *gorm.DB) *RefreshProfileRepository {
	return &RefreshProfileRepository{db: db}
}

func (r *RefreshProfileRepository) List(ctx context.Context) ([]model.RefreshProfile, error) {
	var items []model.RefreshProfile
	if err := r.db.WithContext(ctx).
		Order("created_at desc").
		Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

func (r *RefreshProfileRepository) Get(ctx context.Context, id string) (*model.RefreshProfile, error) {
	var item model.RefreshProfile
	if err := r.db.WithContext(ctx).First(&item, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

func (r *RefreshProfileRepository) Create(ctx context.Context, item *model.RefreshProfile) error {
	return r.db.WithContext(ctx).Create(item).Error
}

func (r *RefreshProfileRepository) Update(ctx context.Context, id string, patch map[string]any) (*model.RefreshProfile, error) {
	patch["updated_at"] = time.Now()
	if err := r.db.WithContext(ctx).
		Model(&model.RefreshProfile{}).
		Where("id = ?", id).
		Updates(patch).Error; err != nil {
		return nil, err
	}
	return r.Get(ctx, id)
}

func (r *RefreshProfileRepository) Delete(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Delete(&model.RefreshProfile{}, "id = ?", id).Error
}

func (r *RefreshProfileRepository) DeleteByIDs(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Delete(&model.RefreshProfile{}, "id IN ?", ids).Error
}

// ListDue returns enabled profiles whose next_retry_at has passed (or is unset,
// e.g. freshly imported). The background maintenance loop refreshes these.
func (r *RefreshProfileRepository) ListDue(ctx context.Context, now time.Time) ([]model.RefreshProfile, error) {
	var items []model.RefreshProfile
	if err := r.db.WithContext(ctx).
		Where("enabled = ? AND (next_retry_at IS NULL OR next_retry_at <= ?)", true, now).
		Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}
