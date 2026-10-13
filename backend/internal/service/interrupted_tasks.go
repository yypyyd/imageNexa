package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"backend/internal/model"
	"backend/internal/netguard"
)

var ErrGenerationOutcomeUnknown = errors.New("generation result could not be recovered; upstream submission may have succeeded; do not automatically resubmit")

const interruptedDispatchAge = 45 * time.Minute

// Reconcile process interruptions without ever replaying the paid submission.
// An allocated object key is not proof of success: inspect the stored bytes.
func (m *MaintenanceService) reconcileInterruptedDispatches(ctx context.Context) {
	ctx, cancelSweep := context.WithTimeout(ctx, 30*time.Second)
	defer cancelSweep()
	if m.models == nil || m.events == nil || m.recovery == nil {
		return
	}
	attempts, err := m.models.Dispatch().ListInterrupted(ctx, time.Now().Add(-interruptedDispatchAge), 50)
	if err != nil {
		recordBookkeepingError("list interrupted dispatches", err)
		return
	}
	for _, attempt := range attempts {
		if ctx.Err() != nil {
			return
		}
		if m.inflight != nil && m.inflight.Active(attempt.EventID) {
			continue
		}
		event, err := m.events.GetByID(ctx, attempt.EventID)
		if err != nil || event == nil || event.Status != "pending" {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		stored, mimeType, err := m.recoverStoredImage(probeCtx, event)
		cancel()
		if err != nil {
			recordBookkeepingError("inspect interrupted artifact", err)
			continue
		}
		if stored {
			recordBookkeepingError("restore completed task", m.models.Dispatch().ResolveInterrupted(ctx, attempt, true, mimeType, ""))
			continue
		}
		if attempt.UpstreamTaskID != "" && event.Kind == "image" && (event.Provider == "chatgpt" || event.Provider == "byteplus") && time.Since(event.TS) < 24*time.Hour {
			if attempt.State != "accepted" {
				recordBookkeepingError("adopt interrupted task", m.models.Dispatch().Finish(ctx, attempt.ID, "accepted", "temporary", attempt.UpstreamTaskID, nil))
			}
			m.recovery.startAcceptedImageRecovery(ctx, event)
			continue
		}
		if attempt.UpstreamTaskID != "" && event.Kind == "video" && (event.Provider == "dola" || event.Provider == "grok") && time.Since(event.TS) < 30*24*time.Hour {
			if attempt.State != "accepted" {
				recordBookkeepingError("adopt interrupted video task", m.models.Dispatch().Finish(ctx, attempt.ID, "accepted", "temporary", attempt.UpstreamTaskID, nil))
			}
			m.recovery.startAcceptedVideoRecovery(ctx, event)
			continue
		}
		// BytePlus's history lookup can still find a task without a handle.
		if event.Provider == "byteplus" && event.Kind == "image" && attempt.UpstreamTaskID == "" && attempt.State == "submitting" {
			recordBookkeepingError("mark interrupted submission unknown", m.models.Dispatch().Finish(ctx, attempt.ID, "unknown", "temporary", "", nil))
			continue
		}
		if event.Provider == "byteplus" && event.Kind == "image" && attempt.State == "unknown" {
			continue
		}
		// 'Unresolved' is terminal to the client, but is NOT proof of no charge.
		// Reservations remain uncertain until an authoritative quota probe settles them.
		recordBookkeepingError("close interrupted task", m.models.Dispatch().ResolveInterrupted(ctx, attempt, false, "", ErrGenerationOutcomeUnknown.Error()))
	}
}

func (m *MaintenanceService) recoverStoredImage(ctx context.Context, event *model.EventLog) (bool, string, error) {
	if event.Kind != "image" || strings.TrimSpace(event.File) == "" || strings.Contains(event.File, "://") || m.store == nil || !m.store.Configured() {
		return false, "", nil
	}
	response, err := m.store.Get(ctx, event.File, "")
	if err != nil {
		return false, "", err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return false, "", nil
	}
	if response.StatusCode != http.StatusOK {
		return false, "", errors.New("artifact check unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, netguard.MaxImageBytes+1))
	if err != nil {
		return false, "", err
	}
	if len(body) == 0 || int64(len(body)) > netguard.MaxImageBytes {
		return false, "", nil
	}
	mimeType, _, err := detectImageArtifact(body)
	return err == nil, mimeType, nil
}
