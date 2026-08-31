package model

import "time"

const (
	APICredentialStatusActive   = "active"
	APICredentialStatusDisabled = "disabled"
	APICredentialStatusRevoked  = "revoked"
)

// APICredential authenticates downstream /v1 callers independently of the
// administrator. Only KeyHash and a non-sensitive preview are persisted; the
// plaintext sk-* value is returned once when the credential is created.
type APICredential struct {
	ID               string     `json:"id" gorm:"primaryKey;size:32"`
	Name             string     `json:"name" gorm:"size:100;not null"`
	KeyPreview       string     `json:"key_preview" gorm:"size:32;not null"`
	KeyHash          string     `json:"-" gorm:"size:255;uniqueIndex;not null"`
	Status           string     `json:"status" gorm:"size:32;index;not null;default:'active'"`
	ConcurrencyLimit int        `json:"concurrency_limit" gorm:"not null;default:0"` // zero means unlimited
	LastUsedAt       *time.Time `json:"last_used_at,omitempty"`
	RevokedAt        *time.Time `json:"revoked_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

func (c APICredential) IsActive() bool {
	return c.Status == APICredentialStatusActive && c.RevokedAt == nil
}
