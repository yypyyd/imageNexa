package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAdminConsoleErrorDoesNotReflectInternalOrProviderErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	secret := "database failed: Cookie: csrfToken=secret; Authorization: Bearer token"

	adminConsoleError(context, errors.New(secret))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "csrfToken") || strings.Contains(recorder.Body.String(), "Bearer") || strings.Contains(recorder.Body.String(), "database failed") {
		t.Fatalf("response leaked internal error: %s", recorder.Body.String())
	}
}

func TestAdminConsoleErrorAllowsExplicitValidationMessage(t *testing.T) {
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)

	adminConsoleError(context, errors.New("weight must be between -1000 and 1000"))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "weight must be between -1000 and 1000") {
		t.Fatalf("validation message missing: %s", recorder.Body.String())
	}
}

func TestSafeAdminValidationMessagesCoverImportAndSettingsInputs(t *testing.T) {
	for _, message := range []string{
		"access_token required",
		"not a runway token",
		"sso token required",
		"not a grok sso token",
		"not an OreateAI cookie",
		"base_url required",
		"base_url and key required",
		"logs_retention_days must be between 1 and 3650",
		"artifacts_retention_days must be between 1 and 3650",
	} {
		if got, ok := safeAdminValidationMessage(errors.New(message)); !ok || got != message {
			t.Errorf("safeAdminValidationMessage(%q) = %q, %v", message, got, ok)
		}
	}
}
