package dola

import (
	"errors"
	"fmt"
)

var ErrTaskAccepted = errors.New("dola: video submitted; do not resubmit")
var ErrTaskSubmissionUnknown = errors.New("dola: submission outcome unknown; do not resubmit")

// AcceptedFailure retains the conversation for operational recovery and charges
// the attempt even if polling or downloading is interrupted.
func AcceptedFailure(conversationID string, cause error) error {
	return &VideoSubmissionError{ConversationID: conversationID, Cause: cause}
}

type VideoSubmissionError struct {
	ConversationID string
	Cause          error
}

func (e *VideoSubmissionError) Error() string {
	return fmt.Sprintf("%v: %v", ErrTaskAccepted, e.Cause)
}

// Deliberately do not unwrap Cause: a later authentication/transport failure
// cannot make an already submitted generation safe to retry or refund.
func (e *VideoSubmissionError) Unwrap() error { return ErrTaskAccepted }
