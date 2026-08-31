package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestCredentialErrorDoesNotReflectInternalDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	writeCredentialError(context, errors.New("postgres failed with sk-secret-value"))
	if recorder.Code != http.StatusInternalServerError || strings.Contains(recorder.Body.String(), "postgres") || strings.Contains(recorder.Body.String(), "sk-secret-value") {
		t.Fatalf("unsafe credential error response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestCredentialErrorAllowsExplicitValidation(t *testing.T) {
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	writeCredentialError(context, errors.New("并发上限不能小于 0"))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "并发上限不能小于 0") {
		t.Fatalf("validation response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
