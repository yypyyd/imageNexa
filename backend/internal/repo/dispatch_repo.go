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
		"finished_at": &now, "updated_at": now,
	}
	if upstreamTaskID != "" {
		patch["upstream_task_id"] = clipDispatchField(upstreamTaskID, 255)
	}
	if cause != nil {
		// Provider errors can contain reflected request bodies, URLs, cookies, or
		// bearer tokens. FailureClass is the durable diagnostic signal; persist a
		// stable class message instead of the raw upstream error text.
		patch["error"] = dispatchFailureMessage(state, failureClass)
	}
	return r.db.WithContext(ctx).Model(&model.DispatchAttempt{}).Where("id = ?", attemptID).Updates(patch).Error
}

// AcceptLatest commits the recovery handle while the provider's stream is still
// open. Dispatches for an event are sequential, including account failover.
func (r *DispatchRepository) AcceptLatest(ctx context.Context, eventID, accountID, routeID, taskID string) error {
	if strings.TrimSpace(taskID) == "" {
		return errors.New("missing accepted task id")
	}
	var attempt model.DispatchAttempt
	if err := r.db.WithContext(ctx).Where("event_id = ? AND account_id = ? AND model_route_id = ? AND state = ?", eventID, accountID, routeID, "submitting").Order("started_at DESC").First(&attempt).Error; err != nil {
		return err
	}
	result := r.db.WithContext(ctx).Model(&model.DispatchAttempt{}).Where("id = ? AND state = ?", attempt.ID, "submitting").Updates(map[string]any{
		"state": "accepted", "upstream_task_id": clipDispatchField(taskID, 255), "updated_at": time.Now(),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("dispatch no longer submitting")
	}
	return nil
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
		Where("event_id = ? AND state IN ? AND upstream_task_id <> ''", strings.TrimSpace(eventID), []string{"accepted", "succeeded"}).
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
		Where("state IN ? AND upstream_task_id <> ''", []string{"accepted", "succeeded"}).
		Where("EXISTS (SELECT 1 FROM event_logs e WHERE e.id = dispatch_attempts.event_id AND e.status = 'pending' AND e.kind = 'image')").
		Order("started_at ASC").Limit(limit).Find(&items).Error
	return items, err
}

// ListAcceptedVideos returns durable video handles that still need their final
// artifact URL. Failed events are included for backwards repair: older builds
// incorrectly marked a polling timeout as a terminal failure even though the
// accepted upstream task continued rendering. Only providers with a durable,
// provider-specific resume handle are eligible here.
func (r *DispatchRepository) ListAcceptedVideos(ctx context.Context, limit int) ([]model.DispatchAttempt, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var items []model.DispatchAttempt
	err := r.db.WithContext(ctx).
		Where("state IN ? AND upstream_task_id <> ''", []string{"accepted", "succeeded"}).
		Where("model_route_id LIKE 'video.%' AND (model_route_id LIKE '%.dola' OR model_route_id LIKE '%.grok')").
		Where("EXISTS (SELECT 1 FROM event_logs e WHERE e.id = dispatch_attempts.event_id AND e.kind = 'video' AND e.provider IN ('dola','grok') AND e.status IN ('pending','failed') AND COALESCE(e.file, '') = '')").
		Order("started_at ASC").Limit(limit).Find(&items).Error
	return items, err
}

// ListInterrupted selects only orphan candidates after the longest live work
// deadline (video: 35 minutes). The service also checks its in-flight registry.
func (r *DispatchRepository) ListInterrupted(ctx context.Context, before time.Time, limit int) ([]model.DispatchAttempt, error) {
	var items []model.DispatchAttempt
	err := r.db.WithContext(ctx).Where("state IN ? AND started_at < ?", []string{"submitting", "unknown", "accepted", "succeeded"}, before).
		Where("EXISTS (SELECT 1 FROM event_logs e WHERE e.id = dispatch_attempts.event_id AND e.status = 'pending')").
		Order("updated_at ASC").Limit(limit).Find(&items).Error
	return items, err
}

// ResolveInterrupted commits the client verdict and dispatch state together so
// another restart cannot strand a pending event behind a terminal attempt.
func (r *DispatchRepository) ResolveInterrupted(ctx context.Context, attempt model.DispatchAttempt, success bool, mimeType, message string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var event model.EventLog
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&event, "id = ?", attempt.EventID).Error; err != nil {
			return err
		}
		if event.Status != "pending" {
			return nil
		}
		events := NewEventRepository(tx)
		state, status, class := "unresolved", "failed", "outcome_unknown"
		var cause error = errors.New(message)
		if success {
			state, status, class, cause = "succeeded", "success", "", nil
			if err := events.SetMimeType(ctx, event.ID, mimeType); err != nil {
				return err
			}
		} else if err := events.SetPhase(ctx, event.ID, "outcome_unknown"); err != nil {
			return err
		}
		if err := events.UpdateStatus(ctx, event.ID, status, message, 0); err != nil {
			return err
		}
		return NewDispatchRepository(tx).Finish(ctx, attempt.ID, state, class, attempt.UpstreamTaskID, cause)
	})
}

// ResolveUnknown closes client waiting and records the evidence for quota
// settlement in one transaction. Only a verified absence permits a refund;
// a timeout leaves both the dispatch and its reservation uncertain. Locking
// and rechecking the attempt prevents a late history probe from overwriting
// another worker's accepted task handle.
func (r *DispatchRepository) ResolveUnknown(ctx context.Context, attempt model.DispatchAttempt, verifiedAbsent bool, message string) (bool, error) {
	applied := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var event model.EventLog
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&event, "id = ?", attempt.EventID).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if event.Status == "success" {
			return nil
		}
		var current model.DispatchAttempt
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", attempt.ID).Error; err != nil {
			return err
		}
		if current.State != "unknown" {
			return nil
		}
		state, class := "unresolved", "outcome_unknown"
		if verifiedAbsent {
			state, class = "failed", "temporary"
		} else if err := tx.Model(&model.QuotaReservation{}).
			Where("dispatch_attempt_id = ? AND status = ?", current.ID, "held").
			Updates(map[string]any{"status": "uncertain", "updated_at": time.Now()}).Error; err != nil {
			return err
		}
		if err := NewDispatchRepository(tx).Finish(ctx, current.ID, state, class, "", errors.New(message)); err != nil {
			return err
		}
		if event.Status == "pending" {
			events := NewEventRepository(tx)
			if !verifiedAbsent {
				if err := events.SetPhase(ctx, event.ID, "outcome_unknown"); err != nil {
					return err
				}
			}
			if err := events.UpdateStatus(ctx, event.ID, "failed", message, 0); err != nil {
				return err
			}
		}
		applied = true
		return nil
	})
	return applied && err == nil, err
}

// ListUnknown returns ambiguous submissions that finished before the cutoff and
// are therefore old enough for the provider's task history to be authoritative.
func (r *DispatchRepository) ListUnknown(ctx context.Context, finishedBefore time.Time, limit int) ([]model.DispatchAttempt, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var items []model.DispatchAttempt
	err := r.db.WithContext(ctx).
		Where("state = ? AND COALESCE(finished_at, started_at) <= ?", "unknown", finishedBefore).
		Where("model_route_id LIKE 'image.%' AND model_route_id LIKE '%.byteplus'").
		Order("started_at ASC").Limit(limit).Find(&items).Error
	return items, err
}

// Adopt upgrades an ambiguous submission to an accepted one once the provider's
// own task history proves the task exists. From then on the attempt follows the
// normal accepted-task recovery path and is never resubmitted.
func (r *DispatchRepository) Adopt(ctx context.Context, attemptID, upstreamTaskID string) error {
	upstreamTaskID = clipDispatchField(upstreamTaskID, 255)
	if upstreamTaskID == "" {
		return errors.New("upstream task id is required to adopt a submission")
	}
	now := time.Now()
	return r.db.WithContext(ctx).Model(&model.DispatchAttempt{}).
		Where("id = ? AND state = ?", attemptID, "unknown").
		Updates(map[string]any{
			"state": "accepted", "failure_class": "temporary",
			"upstream_task_id": upstreamTaskID,
			"error":            dispatchFailureMessage("accepted", "temporary"),
			"finished_at":      nil, "updated_at": now,
		}).Error
}

func dispatchFailureMessage(state, failureClass string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "accepted":
		return "provider task accepted; recovery pending"
	case "unknown":
		return "provider submission outcome unknown"
	case "unresolved":
		return "provider result unavailable; submission may have succeeded; do not resubmit"
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
