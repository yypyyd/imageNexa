package service

import (
	"testing"
	"time"
)

func TestDecideUnknownSubmissionPrefersProviderVerdict(t *testing.T) {
	started := time.Date(2026, 9, 2, 18, 8, 15, 0, time.UTC)
	fresh := started.Add(unknownSubmissionVerifyAfter + time.Second)
	stale := started.Add(unknownSubmissionHardCap + time.Second)

	tests := []struct {
		name    string
		verdict unknownSubmissionVerdict
		now     time.Time
		want    unknownSubmissionAction
	}{
		{name: "history proves the task exists", verdict: unknownSubmissionAccepted, now: fresh, want: adoptUnknownSubmission},
		{name: "history proves the task exists even past the cap", verdict: unknownSubmissionAccepted, now: stale, want: adoptUnknownSubmission},
		{name: "history proves nothing was created", verdict: unknownSubmissionAbsent, now: fresh, want: closeUnknownSubmission},
		{name: "unreadable history waits", verdict: unknownSubmissionUnverified, now: fresh, want: keepUnknownSubmission},
		{name: "unreadable history ages out", verdict: unknownSubmissionUnverified, now: stale, want: closeUnknownSubmission},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideUnknownSubmission(tc.verdict, started, tc.now); got != tc.want {
				t.Fatalf("decideUnknownSubmission() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUnknownSubmissionWindowsCoverCreateTimeout(t *testing.T) {
	// Verification must not start before a slow create_task could still have
	// been accepted upstream, otherwise a genuinely accepted task would be
	// declared absent and closed while the provider is still billing it.
	if unknownSubmissionVerifyAfter < 90*time.Second {
		t.Fatalf("verify delay %v is shorter than the 90s create_task timeout", unknownSubmissionVerifyAfter)
	}
	if unknownSubmissionHardCap <= unknownSubmissionVerifyAfter {
		t.Fatalf("hard cap %v must exceed the verify delay %v", unknownSubmissionHardCap, unknownSubmissionVerifyAfter)
	}
}
