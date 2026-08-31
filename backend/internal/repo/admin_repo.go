package repo

import (
	"context"
	"errors"
	"strings"
	"time"

	"backend/internal/model"
	"gorm.io/gorm"
)

var (
	ErrAdminAlreadyInitialized = errors.New("administrator already initialized")
	ErrAdminInvariant          = errors.New("administrator singleton invariant violated")
)

const adminInitializationLock int64 = 0x3241504941444D49 // "2APIADMI"

type AdminRepository struct {
	db *gorm.DB
}

func NewAdminRepository(db *gorm.DB) *AdminRepository {
	return &AdminRepository{db: db}
}

func (r *AdminRepository) Count(ctx context.Context) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&model.Admin{}).Count(&count).Error
	return count, err
}

func (r *AdminRepository) Initialized(ctx context.Context) (bool, error) {
	count, err := r.Count(ctx)
	if err != nil {
		return false, err
	}
	if count > 1 {
		return false, ErrAdminInvariant
	}
	return count == 1, nil
}

func (r *AdminRepository) Get(ctx context.Context) (*model.Admin, error) {
	var admin model.Admin
	if err := r.db.WithContext(ctx).Where("singleton_key = ?", 1).First(&admin).Error; err != nil {
		return nil, err
	}
	return &admin, nil
}

func (r *AdminRepository) GetByID(ctx context.Context, adminID string) (*model.Admin, error) {
	var admin model.Admin
	if err := r.db.WithContext(ctx).First(&admin, "id = ?", strings.TrimSpace(adminID)).Error; err != nil {
		return nil, err
	}
	return &admin, nil
}

func (r *AdminRepository) GetByIdentifier(ctx context.Context, identifier string) (*model.Admin, error) {
	identifier = strings.ToLower(strings.TrimSpace(identifier))
	if identifier == "" {
		return nil, gorm.ErrRecordNotFound
	}
	var admin model.Admin
	if err := r.db.WithContext(ctx).
		Where("LOWER(username) = ? OR LOWER(email) = ?", identifier, identifier).
		First(&admin).Error; err != nil {
		return nil, err
	}
	return &admin, nil
}

// Initialize serializes the first-admin write with a PostgreSQL transaction
// advisory lock. The table's singleton constraint remains the final backstop.
func (r *AdminRepository) Initialize(ctx context.Context, admin *model.Admin) error {
	if admin == nil {
		return errors.New("administrator is required")
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", adminInitializationLock).Error; err != nil {
			return err
		}

		var count int64
		if err := tx.Model(&model.Admin{}).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return ErrAdminAlreadyInitialized
		}

		admin.ID = model.AdminSingletonID
		admin.SingletonKey = 1
		if admin.Status == "" {
			admin.Status = model.AdminStatusActive
		}
		if admin.SessionVersion < 1 {
			admin.SessionVersion = 1
		}
		return tx.Create(admin).Error
	})
}

func (r *AdminRepository) TouchLogin(ctx context.Context, adminID, ip string) error {
	now := time.Now()
	return r.db.WithContext(ctx).Model(&model.Admin{}).
		Where("id = ?", adminID).
		Updates(map[string]any{"last_login_at": now, "last_login_ip": strings.TrimSpace(ip)}).Error
}

// UpdatePassword also invalidates every existing Redis session through the
// monotonically increasing version embedded in each session payload.
func (r *AdminRepository) UpdatePassword(ctx context.Context, adminID, passwordHash string) error {
	result := r.db.WithContext(ctx).Model(&model.Admin{}).
		Where("id = ?", adminID).
		Updates(map[string]any{
			"password_hash":   passwordHash,
			"session_version": gorm.Expr("session_version + 1"),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}
