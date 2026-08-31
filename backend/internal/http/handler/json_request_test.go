package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestBindAdminJSONRejectsOversizedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(
		http.MethodPost,
		"/admin/api/auth/login",
		strings.NewReader(`{"value":"`+strings.Repeat("x", int(maxAdminJSONBytes))+`"}`),
	)
	context.Request.Header.Set("Content-Type", "application/json")

	var body map[string]any
	if bindAdminJSON(context, &body, "invalid request body") {
		t.Fatal("oversized administrator JSON body was accepted")
	}
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusRequestEntityTooLarge, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "request body too large") {
		t.Fatalf("unexpected response: %s", recorder.Body.String())
	}
}

func TestBindAdminJSONAcceptsSmallBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/admin/api/auth/login", strings.NewReader(`{"username":"admin"}`))
	context.Request.Header.Set("Content-Type", "application/json")

	var body struct {
		Username string `json:"username"`
	}
	if !bindAdminJSON(context, &body, "invalid request body") || body.Username != "admin" {
		t.Fatalf("small administrator JSON body was rejected: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
