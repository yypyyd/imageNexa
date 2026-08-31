package service

import (
	"strings"
	"testing"
)

func TestValidatePasswordEnforcesBcryptSafeBounds(t *testing.T) {
	if err := ValidatePassword("StrongPass!234"); err != nil {
		t.Fatalf("valid password rejected: %v", err)
	}
	for name, password := range map[string]string{
		"under twelve runes":     "Aa1!short",
		"over sixty-four runes":  "Aa1!" + strings.Repeat("x", 61),
		"over bcrypt byte limit": "Aa1!" + strings.Repeat("界", 24),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidatePassword(password); err == nil {
				t.Fatal("password unexpectedly accepted")
			}
		})
	}
}
