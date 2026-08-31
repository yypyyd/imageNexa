package repo

import (
	"context"
	"errors"
	"strings"
	"time"

	"backend/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrAPICredentialRevoked = errors.New("api credential is already revoked")

type APICredentialRepository struct {
	db *gorm.DB
}

func NewAPICredentialRepository(db *gorm.DB) *APICredentialRepository {
	return &APICredentialRepository{db: db}
}

func (r *APICredentialRepository) List(ctx context.Context, includeRevoked bool) ([]model.APICredential, error) {
	var credentials []model.APICredential
	query := r.db.WithContext(ctx).Order("created_at desc, id desc")
	if !includeRevoked {
		query = query.Where("status <> ? AND revoked_at IS NULL", model.APICredentialStatusRevoked)
	}
	if err := query.Find(&credentials).Error; err != nil {
		return nil, err
	}
	return credentials, nil
}

func (r *APICredentialRepository) GetByID(ctx context.Context, credentialID string) (*model.APICredential, error) {
	var credential model.APICredential
	if err := r.db.WithContext(ctx).First(&credential, "id = ?", strings.TrimSpace(credentialID)).Error; err != nil {
		return nil, err
	}
	return &credential, nil
}

func (r *APICredentialRepository) GetActiveByHash(ctx context.Context, keyHash string) (*model.APICredential, error) {
	var credential model.APICredential
	if err := r.db.WithContext(ctx).
		Where("key_hash = ? AND status = ? AND revoked_at IS NULL", keyHash, model.APICredentialStatusActive).
		First(&credential).Error; err != nil {
		return nil, err
	}
	return &credential, nil
}

func (r *APICredentialRepository) Create(ctx context.Context, credential *model.APICredential) error {
	return r.db.WithContext(ctx).Create(credential).Error
}

// Rotate atomically creates the replacement before permanently revoking the
// old key. The row lock ensures two concurrent rotate requests cannot mint two
// active successors.
func (r *APICredentialRepository) Rotate(ctx context.Context, credentialID string, replacement *model.APICredential) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current model.APICredential
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", strings.TrimSpace(credentialID)).Error; err != nil {
			return err
		}
		if current.Status == model.APICredentialStatusRevoked || current.RevokedAt != nil {
			return ErrAPICredentialRevoked
		}
		// Rotating secret material must never reactivate a disabled credential.
		// Copy the status read under this row lock rather than trusting the stale
		// service-layer snapshot used to construct the replacement.
		replacement.Status = current.Status
		if err := tx.Create(replacement).Error; err != nil {
			return err
		}
		now := time.Now()
		return tx.Model(&current).Updates(map[string]any{
			"status": model.APICredentialStatusRevoked, "revoked_at": now, "updated_at": now,
		}).Error
	})
}

func (r *APICredentialRepository) Update(ctx context.Context, credentialID string, patch map[string]any) (*model.APICredential, error) {
	result := r.db.WithContext(ctx).Model(&model.APICredential{}).
		Where("id = ?", strings.TrimSpace(credentialID)).Updates(patch)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, gorm.ErrRecordNotFound
	}
	return r.GetByID(ctx, credentialID)
}

func (r *APICredentialRepository) Revoke(ctx context.Context, credentialID string) error {
	now := time.Now()
	result := r.db.WithContext(ctx).Model(&model.APICredential{}).
		Where("id = ? AND status <> ?", strings.TrimSpace(credentialID), model.APICredentialStatusRevoked).
		Updates(map[string]any{
			"status":     model.APICredentialStatusRevoked,
			"revoked_at": now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := r.db.WithContext(ctx).Model(&model.APICredential{}).
			Where("id = ?", strings.TrimSpace(credentialID)).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return gorm.ErrRecordNotFound
		}
	}
	return nil
}

func (r *APICredentialRepository) TouchLastUsed(ctx context.Context, credentialID string, at time.Time) error {
	return r.db.WithContext(ctx).Model(&model.APICredential{}).
		Where("id = ? AND status = ?", credentialID, model.APICredentialStatusActive).
		UpdateColumn("last_used_at", at).Error
}
