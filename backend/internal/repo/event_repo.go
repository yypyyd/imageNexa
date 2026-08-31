package repo

import (
	"context"
	"errors"
	"strings"
	"time"

	"backend/internal/model"

	"gorm.io/gorm"
)

type EventRepository struct {
	db *gorm.DB
}

func NewEventRepository(db *gorm.DB) *EventRepository {
	return &EventRepository{db: db}
}

func (r *EventRepository) PurgeOlderThan(ctx context.Context, maxAge time.Duration) (int64, error) {
	if maxAge <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-maxAge)
	result := r.db.WithContext(ctx).Where("ts < ?", cutoff).Delete(&model.EventLog{})
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// ClearFiles removes artifact references after media retention deletes the
// corresponding private objects, so administrator log rows cannot retain a
// dangling content URL.
func (r *EventRepository) ClearFiles(ctx context.Context, relPaths []string) (int64, error) {
	if len(relPaths) == 0 {
		return 0, nil
	}
	result := r.db.WithContext(ctx).
		Model(&model.EventLog{}).
		Where("file IN ?", relPaths).
		Updates(map[string]any{"file": "", "updated_at": time.Now()})
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// StaleEvent identifies an abandoned dispatch for in-flight cancellation and
// provider-account health attribution. 2API has no downstream credit refund.
type StaleEvent struct {
	ID        string    `gorm:"column:id"`
	AccountID string    `gorm:"column:account_id"`
	TS        time.Time `gorm:"column:ts"`
}

// PurgeStale marks orphaned pending events as failed and returns the affected
// dispatch identities. skip keeps a generation whose goroutine is still known
// to be alive from being mistaken for an orphan.
func (r *EventRepository) PurgeStale(ctx context.Context, maxAge time.Duration, skip func(StaleEvent) bool) ([]StaleEvent, error) {
	if maxAge <= 0 {
		maxAge = 600 * time.Second
	}
	cutoff := time.Now().Add(-maxAge)
	var stale []StaleEvent
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var candidates []StaleEvent
		query := tx.Model(&model.EventLog{}).
			Where("status = ? AND ts < ?", "pending", cutoff)
		if tx.Migrator().HasTable(&model.DispatchAttempt{}) {
			// A non-idempotent submission may already exist upstream. Accepted and
			// ambiguous attempts stay conservatively pending across restarts; only
			// a same-task recovery or an explicit provider verdict may close them.
			query = query.Where(`NOT EXISTS (
				SELECT 1 FROM dispatch_attempts attempt
				WHERE attempt.event_id = event_logs.id
				  AND attempt.state IN ?
			)`, []string{"accepted", "unknown", "submitting"})
		}
		if err := query.
			Select("id", "account_id", "ts").
			Scan(&candidates).Error; err != nil {
			return err
		}
		ids := make([]string, 0, len(candidates))
		for _, event := range candidates {
			if skip != nil && skip(event) {
				continue
			}
			stale = append(stale, event)
			ids = append(ids, event.ID)
		}
		if len(ids) == 0 {
			return nil
		}
		return tx.Model(&model.EventLog{}).
			Where("status = ? AND id IN ?", "pending", ids).
			Updates(map[string]any{
				"status":     "failed",
				"error":      gorm.Expr("COALESCE(NULLIF(error, ''), ?)", "abandoned (process restarted or request interrupted)"),
				"updated_at": time.Now(),
			}).Error
	})
	if err != nil {
		return nil, err
	}
	return stale, nil
}

func (r *EventRepository) Create(ctx context.Context, item *model.EventLog) error {
	return r.db.WithContext(ctx).Create(item).Error
}

func (r *EventRepository) GetByID(ctx context.Context, id string) (*model.EventLog, error) {
	var event model.EventLog
	if err := r.db.WithContext(ctx).First(&event, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &event, nil
}

func (r *EventRepository) GetImageByRequestID(ctx context.Context, credentialID, requestID string) (*model.EventLog, error) {
	return r.GetByRequestID(ctx, credentialID, "image", requestID)
}

func (r *EventRepository) GetByRequestID(ctx context.Context, credentialID, kind, requestID string) (*model.EventLog, error) {
	var event model.EventLog
	err := r.db.WithContext(ctx).
		Where("api_credential_id = ? AND request_id = ? AND kind = ? AND source = ?", credentialID, requestID, kind, "v1").
		Order("created_at DESC").
		First(&event).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &event, nil
}

func (r *EventRepository) MarkVideoReady(ctx context.Context, eventID, fileURL string, elapsedMS int) error {
	return r.db.WithContext(ctx).
		Model(&model.EventLog{}).
		Where("id = ? AND status <> ?", eventID, "success").
		Updates(map[string]any{
			"status":     "success",
			"file":       fileURL,
			"error":      "",
			"elapsed_ms": elapsedMS,
			"updated_at": time.Now(),
		}).Error
}

func (r *EventRepository) SetFile(ctx context.Context, eventID, fileURL string) error {
	return r.db.WithContext(ctx).
		Model(&model.EventLog{}).
		Where("id = ?", eventID).
		Update("file", fileURL).Error
}

// SetArtifact commits the final object key together with the MIME type detected
// from its bytes. Keeping both columns in one update prevents readers from
// observing a JPEG/WebP/AVIF object under stale PNG metadata.
func (r *EventRepository) SetArtifact(ctx context.Context, eventID, fileURL, mimeType string) error {
	return r.db.WithContext(ctx).
		Model(&model.EventLog{}).
		Where("id = ?", eventID).
		Updates(map[string]any{
			"file":       strings.TrimSpace(fileURL),
			"mime_type":  strings.ToLower(strings.TrimSpace(mimeType)),
			"updated_at": time.Now(),
		}).Error
}

func (r *EventRepository) SetMimeType(ctx context.Context, eventID, mimeType string) error {
	return r.db.WithContext(ctx).
		Model(&model.EventLog{}).
		Where("id = ?", eventID).
		Updates(map[string]any{
			"mime_type":  strings.ToLower(strings.TrimSpace(mimeType)),
			"updated_at": time.Now(),
		}).Error
}

func (r *EventRepository) UpdateStatus(ctx context.Context, eventID, status, errMsg string, elapsedMS int) error {
	patch := map[string]any{
		"status":     status,
		"elapsed_ms": elapsedMS,
		"updated_at": time.Now(),
	}
	if strings.TrimSpace(errMsg) != "" {
		patch["error"] = strings.TrimSpace(errMsg)
	} else if status == "success" {
		patch["error"] = ""
	}
	return r.db.WithContext(ctx).
		Model(&model.EventLog{}).
		Where("id = ? AND status <> ?", eventID, status).
		Updates(patch).Error
}

func (r *EventRepository) SetAccount(ctx context.Context, eventID, accountID, accountEmail string) error {
	return r.db.WithContext(ctx).
		Model(&model.EventLog{}).
		Where("id = ?", eventID).
		Updates(map[string]any{"account_id": accountID, "account_email": accountEmail}).Error
}

func (r *EventRepository) SetProvider(ctx context.Context, eventID, provider string) error {
	return r.db.WithContext(ctx).
		Model(&model.EventLog{}).
		Where("id = ?", eventID).
		Updates(map[string]any{"provider": strings.TrimSpace(provider)}).Error
}

func (r *EventRepository) InFlightByAccount(ctx context.Context) (map[string]int64, error) {
	type row struct {
		AccountID string `gorm:"column:account_id"`
		Count     int64  `gorm:"column:count"`
	}
	var rows []row
	if err := r.db.WithContext(ctx).
		Model(&model.EventLog{}).
		Select("account_id, COUNT(*) AS count").
		Where("status = ? AND account_id <> ''", "pending").
		Group("account_id").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, item := range rows {
		out[item.AccountID] = item.Count
	}
	return out, nil
}
