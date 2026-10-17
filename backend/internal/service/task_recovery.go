package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"backend/internal/model"
	"backend/internal/netguard"
	"backend/internal/provider/byteplus"
	"backend/internal/provider/chatgpt"
	"backend/internal/provider/dola"
	"backend/internal/provider/grok"

	"gorm.io/gorm"
)

// startAcceptedImageRecovery launches one background resume for a durable
// provider task. Unknown submissions have no task id and deliberately do
// nothing here: replaying create_task could double-charge the account.
func (s *V1Service) startAcceptedImageRecovery(parent context.Context, event *model.EventLog) {
	if event == nil || event.Status != "pending" || s.store == nil ||
		(event.Provider != "byteplus" && event.Provider != "chatgpt") ||
		(event.Provider == "byteplus" && s.byteplus == nil) || (event.Provider == "chatgpt" && s.chatgpt == nil) {
		return
	}
	if s.inflight != nil && s.inflight.Active(event.ID) {
		return
	}
	attempt, err := s.models.Dispatch().LatestAcceptedForEvent(parent, event.ID)
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			recordBookkeepingError("load accepted image dispatch", err)
		}
		return
	}
	if strings.TrimSpace(attempt.UpstreamTaskID) == "" {
		return
	}
	if _, loaded := s.imageRecoveries.LoadOrStore(attempt.ID, struct{}{}); loaded {
		return
	}
	go func(event model.EventLog, attempt model.DispatchAttempt) {
		defer s.imageRecoveries.Delete(attempt.ID)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 12*time.Minute)
		defer cancel()
		s.inflight.Add(event.ID, cancel)
		defer s.inflight.Done(event.ID)
		recordBookkeepingError("mark image recovery phase", s.events.SetPhase(ctx, event.ID, "recovery"))

		account, accountErr := s.tokens.Get(ctx, event.Provider, attempt.AccountID)
		if accountErr != nil || account == nil || strings.TrimSpace(account.Value) == "" {
			recordBookkeepingError("load accepted image account", accountErr)
			return
		}
		var data []byte
		var resumeErr error
		if event.Provider == "chatgpt" {
			var meta map[string]any
			_, meta, resumeErr = s.chatgpt.ResumeImageTask(ctx, account.Value, attempt.UpstreamTaskID)
			if resumeErr == nil {
				var body io.ReadCloser
				var ct string
				body, ct, resumeErr = s.chatgpt.OpenAsset(ctx, account.Value, stringValue(meta["image_url"]))
				if resumeErr == nil {
					data, resumeErr = readProviderArtifact(body, ct, netguard.MediaImage, netguard.MaxImageBytes)
				}
			}
		} else {
			data, _, resumeErr = s.byteplus.ResumeImageTask(ctx, account.Value, attempt.UpstreamTaskID, true)
		}
		if resumeErr != nil {
			// A transient poll/download failure keeps the accepted task recoverable.
			// Explicit terminal provider outcomes can safely close the event.
			if errors.Is(resumeErr, byteplus.ErrQuotaExhausted) || errors.Is(resumeErr, byteplus.ErrRiskControl) || errors.Is(resumeErr, byteplus.ErrInvalidParams) || errors.Is(resumeErr, byteplus.ErrTaskFailed) || errors.Is(resumeErr, chatgpt.ErrContentPolicy) {
				publicErr := publicGenerationError(resumeErr)
				if errors.Is(resumeErr, byteplus.ErrTaskFailed) {
					publicErr = ErrProviderExecution
				}
				recordBookkeepingError("finish accepted image event", s.events.UpdateStatus(ctx, event.ID, "failed", publicErr.Error(), 0))
				recordBookkeepingError("finish accepted image dispatch", s.models.Dispatch().Finish(ctx, attempt.ID, "failed", dispatchFailureClassName(resumeErr), attempt.UpstreamTaskID, publicErr))
			}
			return
		}
		if len(data) == 0 {
			return
		}
		objectKey, mimeType, detectErr := recoveredImageArtifact(event.File, event.ID, data)
		if detectErr != nil {
			recordBookkeepingError("reject accepted image artifact", detectErr)
			return
		}
		if metadataErr := s.events.SetArtifact(ctx, event.ID, objectKey, mimeType); metadataErr != nil {
			recordBookkeepingError("update accepted image artifact metadata", metadataErr)
			return
		}
		if putErr := s.store.Put(ctx, objectKey, data, mimeType); putErr != nil {
			recordBookkeepingError("store accepted image", putErr)
			return
		}
		elapsed := int(time.Since(event.TS).Milliseconds())
		if updateErr := s.events.UpdateStatus(ctx, event.ID, "success", "", elapsed); updateErr != nil {
			recordBookkeepingError("complete accepted image event", updateErr)
			return
		}
		recordBookkeepingError("complete accepted image dispatch", s.models.Dispatch().Finish(ctx, attempt.ID, "succeeded", "", attempt.UpstreamTaskID, nil))
		recordBookkeepingError("complete accepted image route", s.models.Routes().RecordAccountRouteResult(ctx, attempt.AccountID, attempt.ModelRouteID, "", true))
		_, updateErr := s.tokens.Update(ctx, event.Provider, account.ID, map[string]any{
			"last_used_at": time.Now(), "success_total": gorm.Expr("success_total + 1"), "fails": 0,
		})
		recordBookkeepingError("complete accepted image account", updateErr)
		_ = s.refreshDispatchQuota(withDispatchRoute(ctx, mustDispatchRoute(ctx, s.models, attempt.ModelRouteID), 0), event.Provider, *account, "image")
		recordBookkeepingError("increment accepted image generation", s.models.IncrementGenerationCount(ctx, event.Model))
	}(*event, *attempt)
}

const acceptedVideoRecoveryWindow = 20 * time.Minute

// startAcceptedVideoRecovery continues an already accepted video task without
// replaying its prompt. Dola conversations and Grok deterministic artifact URLs
// are durable across request timeouts and restarts, so slow renders remain
// pending until this path obtains a final URL or an explicit terminal verdict.
func (s *V1Service) startAcceptedVideoRecovery(parent context.Context, event *model.EventLog) {
	if event == nil || event.Kind != "video" || (event.Status != "pending" && event.Status != "failed") {
		return
	}
	provider := strings.ToLower(strings.TrimSpace(event.Provider))
	if (provider != "dola" && provider != "grok") || (provider == "dola" && s.dola == nil) || (provider == "grok" && s.grok == nil) {
		return
	}
	// The original generation poll may still be alive. Running a second poll is
	// harmless upstream but wastes a residential lease and can race its cleanup.
	if s.inflight != nil && s.inflight.Active(event.ID) {
		return
	}
	attempt, err := s.models.Dispatch().LatestAcceptedForEvent(parent, event.ID)
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			recordBookkeepingError("load accepted video dispatch", err)
		}
		return
	}
	if !strings.HasSuffix(attempt.ModelRouteID, "."+provider) || strings.TrimSpace(attempt.UpstreamTaskID) == "" {
		return
	}
	if _, loaded := s.videoRecoveries.LoadOrStore(attempt.ID, struct{}{}); loaded {
		return
	}
	go func(event model.EventLog, attempt model.DispatchAttempt) {
		defer s.videoRecoveries.Delete(attempt.ID)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), acceptedVideoRecoveryWindow)
		defer cancel()
		s.inflight.Add(event.ID, cancel)
		defer s.inflight.Done(event.ID)
		s.applyGlobalProxy(ctx)
		recordBookkeepingError("mark video recovery phase", s.events.SetPhase(ctx, event.ID, "recovery"))

		account, accountErr := s.tokens.Get(ctx, provider, attempt.AccountID)
		if accountErr != nil || account == nil || strings.TrimSpace(account.Value) == "" {
			recordBookkeepingError("load accepted video account", accountErr)
			return
		}
		var meta map[string]any
		var resumeErr error
		switch provider {
		case "dola":
			meta, resumeErr = s.dola.ResumeVideoTask(ctx, dolaAccountFromToken(*account), attempt.UpstreamTaskID)
		case "grok":
			meta, resumeErr = s.grok.ResumeVideoTask(ctx, account.Value, attempt.UpstreamTaskID)
		}
		if resumeErr != nil {
			if terminalVideoRecoveryError(provider, resumeErr) {
				publicErr := publicGenerationError(resumeErr)
				recordBookkeepingError("finish accepted video event", s.events.UpdateStatus(ctx, event.ID, "failed", publicErr.Error(), 0))
				recordBookkeepingError("finish accepted video dispatch", s.models.Dispatch().Finish(ctx, attempt.ID, "failed", dispatchFailureClassName(resumeErr), attempt.UpstreamTaskID, publicErr))
			}
			return
		}
		videoURL := strings.TrimSpace(stringValue(meta["video_url"]))
		if videoURL == "" {
			return
		}
		elapsed := int(time.Since(event.TS).Milliseconds())
		if updateErr := s.events.MarkVideoReady(ctx, event.ID, videoURL, elapsed); updateErr != nil {
			recordBookkeepingError("complete accepted video event", updateErr)
			return
		}
		recordBookkeepingError("complete accepted video dispatch", s.models.Dispatch().Finish(ctx, attempt.ID, "succeeded", "", attempt.UpstreamTaskID, nil))
		recordBookkeepingError("complete accepted video route", s.models.Routes().RecordAccountRouteResult(ctx, attempt.AccountID, attempt.ModelRouteID, "", true))
		_, updateErr := s.tokens.Update(ctx, provider, account.ID, map[string]any{
			"last_used_at": time.Now(), "success_total": gorm.Expr("success_total + 1"), "fails": 0,
		})
		recordBookkeepingError("complete accepted video account", updateErr)
		recordBookkeepingError("increment accepted video generation", s.models.IncrementGenerationCount(ctx, event.Model))
	}(*event, *attempt)
}

func terminalVideoRecoveryError(provider string, err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch provider {
	case "dola":
		return !errors.Is(err, dola.ErrTemporaryUpstream) && !errors.Is(err, dola.ErrAuth) && !errors.Is(err, dola.ErrChallenge)
	case "grok":
		return errors.Is(err, grok.ErrInvalidTaskHandle) || errors.Is(err, grok.ErrVideoRejected)
	default:
		return false
	}
}

func acceptedVideoRecoveryPending(err error) bool {
	var dolaSubmission *dola.VideoSubmissionError
	if errors.As(err, &dolaSubmission) {
		return !errors.Is(dolaSubmission.Cause, dola.ErrContentRejected) &&
			!errors.Is(dolaSubmission.Cause, dola.ErrVideoFailed) &&
			!errors.Is(dolaSubmission.Cause, dola.ErrVideoNotStarted) &&
			!errors.Is(dolaSubmission.Cause, dola.ErrQuotaExhausted)
	}
	var grokSubmission *grok.VideoSubmissionError
	if errors.As(err, &grokSubmission) {
		return !errors.Is(grokSubmission.Cause, grok.ErrVideoRejected) &&
			!errors.Is(grokSubmission.Cause, grok.ErrQuotaExhausted) &&
			!errors.Is(grokSubmission.Cause, grok.ErrInvalidTaskHandle)
	}
	return false
}

func recoveredImageArtifact(existingKey, eventID string, raw []byte) (string, string, error) {
	contentType, extension, err := detectImageArtifact(raw)
	if err != nil {
		return "", "", err
	}
	objectKey := replaceMediaExtension(existingKey, extension)
	if objectKey == "" {
		objectKey = imageCacheKey(eventID) + extension
	}
	return objectKey, contentType, nil
}
