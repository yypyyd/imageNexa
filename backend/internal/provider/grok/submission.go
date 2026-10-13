package grok

import (
	"errors"
	"strings"
)

// ErrTaskAccepted means Grok may already be rendering the video. Callers must
// resume the durable asset handle and must never submit the prompt again.
var ErrTaskAccepted = errors.New("grok: video submitted; do not resubmit")

// ErrVideoRejected is a definitive response from an already accepted stream.
// It is intentionally distinct from transport errors so recovery bookkeeping
// can close the task instead of polling an asset that will never be created.
var ErrVideoRejected = errors.New("grok: video generation rejected")

// ErrInvalidTaskHandle means durable state is malformed and cannot be polled.
// Authentication and network failures are not terminal: the same handle may be
// retried later after the account or upstream recovers.
var ErrInvalidTaskHandle = errors.New("grok: invalid video recovery handle")

type VideoSubmissionError struct {
	TaskURL string
	Cause   error
}

func (e *VideoSubmissionError) Error() string {
	return ErrTaskAccepted.Error()
}

func (e *VideoSubmissionError) Unwrap() error { return ErrTaskAccepted }

func AcceptedFailure(taskURL string, cause error) error {
	return &VideoSubmissionError{TaskURL: strings.TrimSpace(taskURL), Cause: cause}
}

func AcceptedTaskURL(err error) string {
	var submitted *VideoSubmissionError
	if errors.As(err, &submitted) {
		return strings.TrimSpace(submitted.TaskURL)
	}
	return ""
}
