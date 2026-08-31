package model

import "time"

const (
	AdminSingletonID  = "admin"
	AdminStatusActive = "active"
)

// Admin is the one and only control-plane identity. SingletonKey is fixed to
// one and is protected by a database UNIQUE + CHECK constraint, so application
// races cannot create a second administrator.
type Admin struct {
	ID             string `gorm:"primaryKey;size:32"`
	SingletonKey   int16  `gorm:"column:singleton_key;uniqueIndex;not null;default:1"`
	Username       string `gorm:"size:64;uniqueIndex;not null"`
	Email          string `gorm:"size:255;not null;default:''"`
	PasswordHash   string `gorm:"size:255;not null"`
	Status         string `gorm:"size:32;index;not null;default:'active'"`
	SessionVersion int64  `gorm:"not null;default:1"`
	LastLoginAt    *time.Time
	LastLoginIP    string `gorm:"size:128"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (a Admin) IsActive() bool {
	return a.Status == AdminStatusActive
}
