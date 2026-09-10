package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"backend/internal/provider/adobe"
	"backend/internal/provider/byteplus"
	"backend/internal/provider/chatgpt"
	"backend/internal/provider/custom"
	"backend/internal/provider/dola"
	"backend/internal/provider/grok"
	"backend/internal/provider/oreate"
	"backend/internal/provider/runway"
)

var ErrDolaDurationRejected = fmt.Errorf("%w: Dola 上游拒绝本次 30 秒请求，未启动生成，未扣账号次数", ErrUnsupportedParams)
var ErrDolaCopyrightRejected = errors.New("dola 因版权保护拒绝返回生成的视频（710092006）")
var ErrDolaAudioCopyrightRejected = errors.New("dola 因生成视频的音频版权检查拒绝返回视频（710092007）")

// publicGenerationError collapses provider-specific failures into stable,
// credential-free API categories. Provider errors routinely contain reflected
// response bodies, URLs, paths, prompts, cookies, or bearer tokens and must not
// be persisted in EventLog or returned by asynchronous polling endpoints.
func publicGenerationError(cause error) error {
	if cause == nil {
		return nil
	}
	// Display a safe terminal content rejection even when the submission wrapper
	// deliberately hides its cause from the retry/quota classifier.
	var submitted *dola.VideoSubmissionError
	dolaCause := cause
	if errors.As(cause, &submitted) {
		dolaCause = submitted.Cause
	}
	if errors.Is(cause, ErrDolaAudioCopyrightRejected) || errors.Is(dolaCause, dola.ErrAudioCopyrightRejected) {
		return ErrDolaAudioCopyrightRejected
	}
	if errors.Is(cause, ErrDolaCopyrightRejected) || errors.Is(dolaCause, dola.ErrCopyrightRejected) {
		return ErrDolaCopyrightRejected
	}
	if errors.As(cause, &submitted) && errors.Is(submitted.Cause, dola.ErrContentRejected) {
		return ErrContentRejected
	}
	switch {
	case errors.Is(cause, dola.ErrVideoNotStarted):
		return ErrDolaDurationRejected
	case errors.Is(cause, ErrIdempotencyConflict):
		return ErrIdempotencyConflict
	case errors.Is(cause, ErrUnknownModel):
		return ErrUnknownModel
	case errors.Is(cause, ErrReferenceTooLarge):
		return ErrReferenceTooLarge
	case errors.Is(cause, ErrReferenceVideoTooLarge):
		return ErrReferenceVideoTooLarge
	case errors.Is(cause, ErrReferenceAudioTooLarge):
		return ErrReferenceAudioTooLarge
	case errors.Is(cause, ErrBannedPrompt):
		return ErrBannedPrompt
	case errors.Is(cause, ErrUnsupportedParams), errors.Is(cause, byteplus.ErrInvalidParams), errors.Is(cause, custom.ErrBadRequest):
		return ErrUnsupportedParams
	case errors.Is(cause, oreate.ErrAccountChallenge), errors.Is(cause, oreate.ErrSpamUser):
		return ErrProviderTemporary
	case errors.Is(cause, ErrContentRejected), errors.Is(cause, dola.ErrContentRejected), errors.Is(cause, byteplus.ErrRiskControl),
		errors.Is(cause, chatgpt.ErrContentPolicy), errors.Is(cause, oreate.ErrContentRejected), errors.Is(cause, oreate.ErrRiskControl):
		return ErrContentRejected
	case errors.Is(cause, ErrNoProviderAccount):
		return ErrNoProviderAccount
	case errors.Is(cause, ErrConcurrencyBackendUnavailable):
		return ErrConcurrencyBackendUnavailable
	case errors.Is(cause, ErrUserConcurrencyFull):
		return ErrUserConcurrencyFull
	case errors.Is(cause, ErrConcurrencyFull):
		return ErrConcurrencyFull
	case errors.Is(cause, ErrProviderAuth), errors.Is(cause, adobe.ErrAuth), errors.Is(cause, adobe.ErrEntitlement),
		errors.Is(cause, byteplus.ErrAuth), errors.Is(cause, chatgpt.ErrAuth), errors.Is(cause, runway.ErrAuth),
		errors.Is(cause, grok.ErrAuth), errors.Is(cause, oreate.ErrAuth), errors.Is(cause, dola.ErrAuth), errors.Is(cause, custom.ErrAuth):
		return ErrProviderAuth
	case errors.Is(cause, ErrProviderQuota), errors.Is(cause, adobe.ErrQuotaExhausted),
		errors.Is(cause, byteplus.ErrQuotaExhausted), errors.Is(cause, chatgpt.ErrQuotaExhausted),
		errors.Is(cause, runway.ErrQuotaExhausted), errors.Is(cause, grok.ErrQuotaExhausted),
		errors.Is(cause, oreate.ErrQuotaExhausted), errors.Is(cause, dola.ErrQuotaExhausted), errors.Is(cause, custom.ErrQuotaExhausted):
		return ErrProviderQuota
	case errors.Is(cause, ErrProviderTemporary), errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded),
		errors.Is(cause, adobe.ErrTemporaryUpstream), errors.Is(cause, byteplus.ErrTemporaryUpstream),
		errors.Is(cause, chatgpt.ErrTemporaryUpstream), errors.Is(cause, runway.ErrTemporaryUpstream),
		errors.Is(cause, grok.ErrTemporaryUpstream), errors.Is(cause, oreate.ErrTemporaryUpstream), errors.Is(cause, dola.ErrTemporaryUpstream),
		errors.Is(cause, custom.ErrTemporaryUpstream):
		return ErrProviderTemporary
	case errors.Is(cause, ErrProviderUnsupported):
		return ErrProviderUnsupported
	default:
		return ErrProviderExecution
	}
}

func safeGenerationErrorText(cause error) string {
	if safe := publicGenerationError(cause); safe != nil {
		return safe.Error()
	}
	return ""
}

// safeStoredGenerationError protects polling/admin reads of rows created by an
// older build that may have persisted raw provider text. Only exact messages
// emitted by this service are allowed through; everything else is collapsed.
func safeStoredGenerationError(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	for _, safe := range []error{
		ErrDolaDurationRejected, ErrDolaCopyrightRejected, ErrDolaAudioCopyrightRejected,
		ErrUnknownModel, ErrIdempotencyConflict, ErrInsufficientFunds, ErrUnsupportedParams,
		ErrBannedPrompt, ErrContentRejected, ErrNoProviderAccount,
		ErrProviderAuth, ErrProviderQuota, ErrProviderTemporary, ErrProviderExecution,
		ErrProviderUnsupported,
		ErrConcurrencyBackendUnavailable, ErrConcurrencyFull, ErrUserConcurrencyFull,
		ErrReferenceTooLarge, ErrReferenceVideoTooLarge, ErrReferenceAudioTooLarge,
	} {
		if raw == safe.Error() {
			return raw
		}
	}
	return ErrProviderExecution.Error()
}

func recordBookkeepingError(operation string, err error) {
	if err != nil {
		// Database/transport errors can embed DSNs, signed URLs, provider bodies,
		// or credentials. The operation label is a fixed call-site constant and is
		// sufficient for alerting; never log the raw error here.
		log.Printf("generation bookkeeping %s failed", operation)
	}
}

func safeQuotaProbeError(data map[string]any) any {
	if data == nil || strings.TrimSpace(stringValue(data["error"])) == "" {
		return nil
	}
	return "provider quota probe failed"
}
