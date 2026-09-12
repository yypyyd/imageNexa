package model

import (
	"strings"
	"time"

	"gorm.io/datatypes"
)

// BannedWord is an admin-managed prompt blocklist entry. Generation requests
// whose prompt contains Word (case-insensitive substring) are rejected before
// reaching any provider; Hits counts how many requests each word blocked.
type BannedWord struct {
	ID        string `gorm:"primaryKey;size:32"`
	Word      string `gorm:"size:255;uniqueIndex;not null"`
	Hits      int64  `gorm:"not null;default:0"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// BannedWordHit records one blocked request: which word matched, who sent it,
// and when. Feeds the admin 违禁词触发列表.
type BannedWordHit struct {
	ID        string    `gorm:"primaryKey;size:32"`
	WordID    string    `gorm:"size:32;index"`
	Word      string    `gorm:"size:255;index;not null"`
	UserID    string    `gorm:"size:32;index"`
	UserName  string    `gorm:"size:255"` // snapshot of name/email at hit time
	Prompt    string    `gorm:"type:text"`
	CreatedAt time.Time `gorm:"index"`
}

type EventLog struct {
	ID                 string `gorm:"primaryKey;size:32"`
	APICredentialID    string `gorm:"size:32;index"`
	RequestID          string `gorm:"size:191;index"`
	RequestFingerprint string `gorm:"size:64"`
	// ResponseFormat preserves the OpenAI image response contract across an
	// accepted upstream task and a later /v1/images/tasks recovery.
	ResponseFormat string `gorm:"size:16;not null;default:''"`
	// MimeType is the payload-magic-detected artifact type, never a filename or
	// upstream Content-Type guess.
	MimeType   string         `gorm:"size:100;not null;default:''"`
	TS         time.Time      `gorm:"index;not null"`
	Kind       string         `gorm:"size:32;index;not null"`
	Status     string         `gorm:"size:32;index;not null"`
	Model      string         `gorm:"size:255;index"`
	Provider   string         `gorm:"size:100;index"`
	Prompt     string         `gorm:"type:text"`
	Ratio      string         `gorm:"size:32"`
	Resolution string         `gorm:"size:32"`
	Duration   string         `gorm:"size:32"`
	Refs       int            `gorm:"not null;default:0"`
	DeAI       bool           `gorm:"not null;default:false"` // 去AI特征 was applied (image only)
	RefFiles   datatypes.JSON `gorm:"type:jsonb"`             // relative paths of saved reference images, for回显 on reload
	Source     string         `gorm:"size:32;index"`
	// AccountID is the provider token/account chosen to fulfil this generation,
	// stamped when the upstream call begins. Drives the accounts view's live
	// in-flight count (pending events per account) and lets an abandoned-event
	// purge attribute the failure back to the account it was using.
	AccountID string `gorm:"size:64;index"`
	// AccountEmail denormalizes the account's email at stamp time, so log rows
	// keep showing which mailbox fulfilled the generation even after the account
	// is deleted or re-imported under a different ID.
	AccountEmail string  `gorm:"size:255"`
	Cost         float64 `gorm:"not null;default:0"`
	ElapsedMS    int     `gorm:"not null;default:0"`
	File         string  `gorm:"size:500;index"`
	Error        string  `gorm:"type:text"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type ModelConfig struct {
	ID             string            `gorm:"primaryKey;size:255"`
	Type           string            `gorm:"size:32;index;not null"`
	Name           string            `gorm:"size:255;not null"`
	Alias          string            `gorm:"column:alias;size:255;not null;default:''"`
	Provider       string            `gorm:"size:100;index;not null"`
	Enabled        bool              `gorm:"not null;default:true"`
	Ratios         datatypes.JSON    `gorm:"type:jsonb"`
	Prices         datatypes.JSONMap `gorm:"type:jsonb"`
	Resolutions    datatypes.JSON    `gorm:"type:jsonb"`
	ImageToImage   bool              `gorm:"not null;default:false"`
	DurationPrices datatypes.JSONMap `gorm:"type:jsonb"`
	// Agent (代理) pricing — optional overlay over Prices/DurationPrices. A tier
	// left unset here means agent users pay the normal price for that tier; the
	// set of *supported* tiers is always driven by Prices, not these.
	PricesAgent         datatypes.JSONMap `gorm:"type:jsonb;column:prices_agent"`
	DurationPricesAgent datatypes.JSONMap `gorm:"type:jsonb;column:duration_prices_agent"`
	Durations           datatypes.JSON    `gorm:"type:jsonb"`
	MaxReferenceImages  int               `gorm:"not null;default:0"`
	MaxReferenceVideos  int               `gorm:"not null;default:0"`
	MaxReferenceAudios  int               `gorm:"not null;default:0"`
	MaxReferenceMedia   int               `gorm:"not null;default:0"`
	SupportsAudioOutput bool              `gorm:"not null;default:false"`
	ReferenceMode       string            `gorm:"size:32;not null;default:'none'"`
	// Custom-upstream models (provider="custom"): UpstreamModel is the model name
	// sent to the upstream OpenAI-compatible API; the base_url + key live on the
	// matching custom account (pool="custom", meta.base_url). Empty for built-ins.
	UpstreamModel string `gorm:"size:255;not null;default:''"`
	// Weight controls display order in the model list: higher weights come first;
	// ties fall back to created_at descending. Default 0.
	Weight int `gorm:"not null;default:0;index"`
	// GenerationCount is a persistent success counter, incremented once per
	// successful generation. Independent of the event_log (which is subject to
	// retention / manual clearing), so the admin "次数" is a true running total.
	GenerationCount int64 `gorm:"not null;default:0"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (m ModelConfig) EffectiveName() string {
	if strings.TrimSpace(m.Alias) != "" {
		return strings.TrimSpace(m.Alias)
	}
	return m.ID
}

type TokenAccount struct {
	ID              string `gorm:"primaryKey;size:64"`
	Pool            string `gorm:"size:64;index;not null"`
	Value           string `gorm:"type:text;not null;default:''"`
	IdentityHash    string `gorm:"column:identity_hash;size:64;not null;default:''" json:"-"`
	ARPSessionToken string `gorm:"column:arp_session_token;type:text;not null;default:''" json:"-"`
	Status          string `gorm:"size:32;index;not null"`
	Fails           int    `gorm:"not null;default:0"`
	FailTotal       int    `gorm:"not null;default:0"`
	// Provider-side failures (overload / 5xx) are tracked apart
	// from Fails/FailTotal: every account fails the same way during an upstream
	// outage, so they say nothing about this account's health.
	UpstreamFails         int               `gorm:"not null;default:0"`
	SuccessTotal          int               `gorm:"not null;default:0"`
	Dead                  bool              `gorm:"not null;default:false"`
	Meta                  datatypes.JSONMap `gorm:"type:jsonb"`
	AddedAt               *time.Time
	LastUsedAt            *time.Time
	CachedQuotaResetAfter string `gorm:"size:128"`
	QuotaRecoverAt        *time.Time
	// Adobe quota is tracked separately for image vs video. An account only
	// enters the shared "quota" waiting status when BOTH are limited; a single
	// limit leaves the account usable for the other kind. Recovery time is shared
	// (QuotaRecoverAt / CachedQuotaResetAfter) since Adobe resets both at once.
	ImageLimited       bool   `gorm:"not null;default:false"`
	VideoLimited       bool   `gorm:"not null;default:false"`
	AccountEmail       string `gorm:"size:255"`
	AccountDisplayName string `gorm:"size:255"`
	// Weight biases scheduling order for ANY account — higher weight is picked
	// first within its pool (ties fall back to round-robin). Default 0.
	Weight int `gorm:"not null;default:0"`
	// Concurrency is the max simultaneous jobs for THIS account. Custom upstreams
	// and Adobe accounts honor it; other built-in pools use their system default.
	// 0 = use the pool default (Adobe points 4, ordinary pools 1, Grok 10).
	Concurrency int `gorm:"not null;default:0"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// SchedulingStub is set when credential columns were omitted from a
	// scheduling query. An empty Value then means "not loaded", not "missing".
	SchedulingStub bool `gorm:"-" json:"-"`
}

type RefreshProfile struct {
	ID                  string `gorm:"primaryKey;size:64"`
	Name                string `gorm:"size:255;not null"`
	Pool                string `gorm:"size:64;index;not null"`
	Kind                string `gorm:"size:64;index;not null"`
	Cookie              string `gorm:"type:text"`
	ARPSessionToken     string `gorm:"column:arp_session_token;type:text;not null;default:''" json:"-"`
	LoginIdentity       string `gorm:"column:login_identity;type:text;not null;default:''" json:"-"`
	LoginSecret         string `gorm:"column:login_secret;type:text;not null;default:''" json:"-"`
	Enabled             bool   `gorm:"not null;default:true"`
	IntervalSeconds     int    `gorm:"not null;default:54000"`
	ImportedAt          *time.Time
	LastAttemptAt       *time.Time
	LastSuccessAt       *time.Time
	LastError           string `gorm:"type:text"`
	NextRetryAt         *time.Time
	ConsecutiveFailures int `gorm:"not null;default:0"`
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type SiteSetting struct {
	Key       string `gorm:"primaryKey;size:100"`
	Value     string `gorm:"type:text"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AutoMigrateModels contains only retained operational tables. Identity,
// provider-account, routing, quota, and dispatch tables are owned exclusively
// by checksummed SQL migrations; adding them here can silently relax constraints
// or change PostgreSQL column types on startup.
func AutoMigrateModels() []any {
	return []any{
		&BannedWord{},
		&BannedWordHit{},
		&EventLog{},
		&RefreshProfile{},
		&SiteSetting{},
	}
}
