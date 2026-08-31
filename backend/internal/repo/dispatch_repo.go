package repo

import (
	"context"
	"strings"
	"time"

	"backend/internal/model"
	"gorm.io/gorm"
)

type DispatchRepository struct{ db *gorm.DB }

func NewDispatchRepository(db *gorm.DB) *DispatchRepository { return &DispatchRepository{db: db} }

func (r *DispatchRepository) Start(ctx context.Context, eventID, routeID, accountID string) (*model.DispatchAttempt, error) {
	now := time.Now()
	item := &model.DispatchAttempt{ID: durableID("dsp_"), EventID: eventID, ModelRouteID: routeID,
		AccountID: accountID, State: "submitting", StartedAt: now}
	if err := r.db.WithContext(ctx).Create(item).Error; err != nil {
		return nil, err
	}
	return item, nil
}

func (r *DispatchRepository) SetAccount(ctx context.Context, attemptID, accountID string) error {
	return r.db.WithContext(ctx).Model(&model.DispatchAttempt{}).Where("id = ?", attemptID).
		Updates(map[string]any{"account_id": accountID, "updated_at": time.Now()}).Error
}

func (r *DispatchRepository) Finish(ctx context.Context, attemptID, state, failureClass, upstreamTaskID string, cause error) error {
	now := time.Now()
	patch := map[string]any{
		"state": state, "failure_class": failureClass,
		"upstream_task_id": clipDispatchField(upstreamTaskID, 255),
		"finished_at":      &now, "updated_at": now,
	}
	if cause != nil {
		// Provider errors can contain reflected request bodies, URLs, cookies, or
		// bearer tokens. FailureClass is the durable diagnostic signal; persist a
		// stable class message instead of the raw upstream error text.
		patch["error"] = dispatchFailureMessage(state, failureClass)
	}
	return r.db.WithContext(ctx).Model(&model.DispatchAttempt{}).Where("id = ?", attemptID).Updates(patch).Error
}

func (r *DispatchRepository) Get(ctx context.Context, attemptID string) (*model.DispatchAttempt, error) {
	var item model.DispatchAttempt
	if err := r.db.WithContext(ctx).First(&item, "id = ?", attemptID).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

// LatestAcceptedForEvent finds the durable recovery handle for an event whose
// non-idempotent provider submission succeeded but whose poll/download did not.
// Unknown submissions deliberately have no task id and are never returned: a
// caller must not guess or resubmit them.
func (r *DispatchRepository) LatestAcceptedForEvent(ctx context.Context, eventID string) (*model.DispatchAttempt, error) {
	var item model.DispatchAttempt
	err := r.db.WithContext(ctx).
		Where("event_id = ? AND state = ? AND upstream_task_id <> ''", strings.TrimSpace(eventID), "accepted").
		Order("started_at DESC").First(&item).Error
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (r *DispatchRepository) ListAccepted(ctx context.Context, limit int) ([]model.DispatchAttempt, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var items []model.DispatchAttempt
	err := r.db.WithContext(ctx).
		Where("state = ? AND upstream_task_id <> ''", "accepted").
		Order("started_at ASC").Limit(limit).Find(&items).Error
	return items, err
}

func dispatchFailureMessage(state, failureClass string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "accepted":
		return "provider task accepted; recovery pending"
	case "unknown":
		return "provider submission outcome unknown"
	}
	switch strings.ToLower(strings.TrimSpace(failureClass)) {
	case "auth", "entitlement", "authorization":
		return "provider authorization failed"
	case "quota":
		return "provider quota exhausted"
	case "temporary":
		return "provider temporarily unavailable"
	case "content":
		return "provider rejected the content"
	case "concurrency":
		return "provider account concurrency exhausted"
	case "request":
		return "provider rejected the request"
	default:
		return "provider request failed"
	}
}

func clipDispatchField(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit > 0 && len(value) > limit {
		return value[:limit]
	}
	return value
}
