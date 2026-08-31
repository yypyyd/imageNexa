package service

import (
	"encoding/json"
	"testing"
	"time"
)

func TestBytePlusCredentialSecretAcceptsOnlyCookieMaterial(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "raw cookie header",
			input: "Cookie: sessionid=s1; csrfToken=c1; passport_auth_status=enabled",
			want:  "sessionid=s1; csrfToken=c1; passport_auth_status=enabled",
		},
		{
			name:  "cookie string export",
			input: `{"cookie_string":"sessionid=s2; csrfToken=c2; passport_auth_status=enabled","account":{"email":"ignored@example.com"},"credits":999999}`,
			want:  "sessionid=s2; csrfToken=c2; passport_auth_status=enabled",
		},
		{
			name:  "cookies array export",
			input: `{"cookies":[{"name":"sessionid","value":"s3","domain":".byteplus.com"},{"name":"csrfToken","value":"c3"},{"name":"passport_auth_status","value":"enabled"}],"tenant":{"id":"ignored"}}`,
			want:  "sessionid=s3; csrfToken=c3; passport_auth_status=enabled",
		},
		{
			name:  "profile fields are not credentials",
			input: `{"account":{"email":"ignored@example.com"},"tenant":"ignored","credits":999,"token":"must-not-be-trusted","cookie":"must-not-be-trusted"}`,
			want:  "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var value any = test.input
			var decoded any
			if json.Unmarshal([]byte(test.input), &decoded) == nil {
				value = decoded
			}
			got := bytePlusCredentialSecret(value, credentialMap(value))
			if got != test.want {
				t.Fatalf("bytePlusCredentialSecret() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestTrustedQuotaSnapshotDoesNotTurnUnknownIntoZero(t *testing.T) {
	for _, snapshot := range []map[string]any{
		{"unknown": true, "remaining": 0, "total": 0},
		{"auth_failed": true, "remaining": 0},
		{"unknown": false, "remaining": nil},
	} {
		remaining, total, resetAt, trusted := trustedQuotaSnapshot(snapshot)
		if trusted || remaining != nil || total != nil || resetAt != nil {
			t.Fatalf("unknown snapshot was trusted: %#v", snapshot)
		}
	}

	remaining, total, resetAt, trusted := trustedQuotaSnapshot(map[string]any{
		"unknown": false, "remaining": 0, "total": 80,
		"available_until": "2026-09-02T12:00:00Z",
	})
	if !trusted || remaining == nil || *remaining != 0 || total == nil || *total != 80 {
		t.Fatalf("definitive zero was not preserved: remaining=%v total=%v trusted=%v", remaining, total, trusted)
	}
	wantReset, _ := time.Parse(time.RFC3339, "2026-09-02T12:00:00Z")
	if resetAt == nil || !resetAt.Equal(wantReset) {
		t.Fatalf("resetAt = %v, want %v", resetAt, wantReset)
	}
}
