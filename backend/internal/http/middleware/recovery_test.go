package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRecoveryDoesNotLogRequestSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var logs bytes.Buffer
	engine := gin.New()
	engine.Use(RequestID(), Recovery(&logs))
	engine.GET("/v1/panic", func(*gin.Context) { panic("panic payload must stay private") })

	request := httptest.NewRequest(http.MethodGet, "/v1/panic", nil)
	request.Header.Set("X-Request-Id", "safe-request-id")
	request.Header.Set("Authorization", "Bearer sk-secret-api-key")
	request.Header.Set("Cookie", "twoapi_admin_session=secret-session")
	request.Header.Set(CSRFHeaderName, "secret-csrf-token")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	logged := logs.String()
	for _, secret := range []string{
		"sk-secret-api-key",
		"secret-session",
		"secret-csrf-token",
		"panic payload must stay private",
		"Authorization",
		"Cookie",
	} {
		if strings.Contains(logged, secret) {
			t.Fatalf("recovery log disclosed %q: %s", secret, logged)
		}
	}
	for _, field := range []string{
		`request_id="safe-request-id"`,
		`method="GET"`,
		`path="/v1/panic"`,
		"panic_type=string",
	} {
		if !strings.Contains(logged, field) {
			t.Fatalf("recovery log missing %q: %s", field, logged)
		}
	}
	if strings.Contains(recorder.Body.String(), "panic payload") {
		t.Fatalf("response disclosed panic value: %s", recorder.Body.String())
	}
}
