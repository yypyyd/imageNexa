package service

import (
	"context"
	"errors"
	"testing"

	"backend/internal/model"
	"backend/internal/provider/dola"
)

func TestDolaVideoFailureIsTerminalAndRetainsAcceptedHandle(t *testing.T) {
	for _, failure := range []error{dola.ErrVideoFailed, dola.ErrVideoPlanningFailed} {
		err := dola.AcceptedFailure("conversation-1", failure)
		if !errors.Is(err, dola.ErrTaskAccepted) || dispatchUpstreamTaskID(err) != "conversation-1" {
			t.Fatal("lost accepted task handle or quota settlement boundary")
		}
		if auth, quota, temporary, dead := dolaErrClass(err); auth || quota || temporary || dead {
			t.Fatal("a failed conversation must not disable or retry the account")
		}
		if !terminalVideoRecoveryError("dola", failure) {
			t.Fatal("final Dola failure must stop recovery")
		}
	}
	for _, retryable := range []error{dola.ErrTemporaryUpstream, dola.ErrAuth, dola.ErrChallenge, context.DeadlineExceeded} {
		if terminalVideoRecoveryError("dola", retryable) {
			t.Fatalf("read failure must stay recoverable: %v", retryable)
		}
	}
}

func TestDolaPlanningFailurePublicMapping(t *testing.T) {
	for _, cause := range []error{dola.ErrVideoPlanningFailed, dola.AcceptedFailure("conversation-1", dola.ErrVideoPlanningFailed), ErrDolaVideoPlanningFailed} {
		publicErr := publicGenerationError(cause)
		if !errors.Is(publicErr, ErrDolaVideoPlanningFailed) || safeStoredGenerationError(publicErr.Error()) != ErrDolaVideoPlanningFailed.Error() {
			t.Fatalf("lost planning failure: %v", publicErr)
		}
	}
}

func TestDolaVideoFailureSurvivesPublicAndStoredErrorMapping(t *testing.T) {
	for _, cause := range []error{dola.ErrVideoFailed, dola.AcceptedFailure("conversation-1", dola.ErrVideoFailed), ErrDolaVideoFailed} {
		publicErr := publicGenerationError(cause)
		if !errors.Is(publicErr, ErrDolaVideoFailed) || safeStoredGenerationError(publicErr.Error()) != ErrDolaVideoFailed.Error() {
			t.Fatalf("lost public failure: %v", publicErr)
		}
	}
	job := (&V1Service{}).videoJobFromEvent(&model.EventLog{
		ID: "event-1", Kind: "video", Provider: "dola", Status: "failed", Error: ErrDolaVideoFailed.Error(),
	})
	if job["status"] != "failed" {
		t.Fatalf("failed event still appears pending: %#v", job)
	}
	failure, ok := job["error"].(map[string]any)
	if !ok || failure["message"] != ErrDolaVideoFailed.Error() {
		t.Fatalf("polling response lost failure message: %#v", job["error"])
	}
}
