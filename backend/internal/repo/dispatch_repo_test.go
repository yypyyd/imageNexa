package repo

import (
	"strings"
	"testing"
)

func TestDispatchFailureMessageIsStableAndCredentialFree(t *testing.T) {
	for state, class := range map[string]string{
		"failed":   "auth",
		"unknown":  "temporary",
		"accepted": "temporary",
	} {
		got := dispatchFailureMessage(state, class)
		if got == "" || strings.Contains(strings.ToLower(got), "bearer") || strings.Contains(strings.ToLower(got), "cookie") {
			t.Fatalf("dispatch message = %q", got)
		}
	}
}
