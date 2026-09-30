package service

import (
	"encoding/base64"
	"fmt"
	"testing"
	"time"
)

func TestChatGPTTokenExpired(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	token := func(exp int64) string {
		payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp)))
		return "header." + payload + ".signature"
	}
	for _, test := range []struct {
		name    string
		value   string
		expired bool
	}{
		{"past", token(now.Unix() - 1), true},
		{"at expiry", token(now.Unix()), true},
		{"future", token(now.Unix() + 1), false},
		{"opaque", "opaque-token", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := chatGPTTokenExpired(test.value, now); got != test.expired {
				t.Fatalf("expired = %v, want %v", got, test.expired)
			}
		})
	}
}
