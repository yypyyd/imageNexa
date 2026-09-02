package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"backend/internal/config"
	"backend/internal/http/handler"
	"github.com/gin-gonic/gin"
)

func TestAdminCORSAllowsBootstrapTokenHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		AppEnv:      "development",
		CORSOrigins: []string{"https://admin.example.test"},
	}
	engine := New(cfg, nil, nil, Handlers{
		Health:         (*handler.HealthHandler)(nil),
		V1:             (*handler.V1Handler)(nil),
		Auth:           (*handler.AuthHandler)(nil),
		APICredentials: (*handler.UserToolsHandler)(nil),
		Admin:          (*handler.AdminConsoleHandler)(nil),
		BannedWords:    (*handler.BannedWordsHandler)(nil),
	})

	request := httptest.NewRequest(http.MethodOptions, "/admin/api/auth/initialize", nil)
	request.Header.Set("Origin", "https://admin.example.test")
	request.Header.Set("Access-Control-Request-Method", http.MethodPost)
	request.Header.Set("Access-Control-Request-Headers", strings.ToLower(handler.AdminBootstrapTokenHeader))
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want %d: %s", recorder.Code, http.StatusNoContent, recorder.Body.String())
	}
	if got := strings.ToLower(recorder.Header().Get("Access-Control-Allow-Headers")); !strings.Contains(got, strings.ToLower(handler.AdminBootstrapTokenHeader)) {
		t.Fatalf("Access-Control-Allow-Headers = %q, missing bootstrap token header", got)
	}
}

func TestV1CORSPreflightDoesNotRequireBearerCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := New(&config.Config{
		AppEnv:      "development",
		CORSOrigins: []string{"https://admin.example.test"},
	}, nil, nil, Handlers{
		Health:         (*handler.HealthHandler)(nil),
		V1:             (*handler.V1Handler)(nil),
		Auth:           (*handler.AuthHandler)(nil),
		APICredentials: (*handler.UserToolsHandler)(nil),
		Admin:          (*handler.AdminConsoleHandler)(nil),
		BannedWords:    (*handler.BannedWordsHandler)(nil),
	})

	request := httptest.NewRequest(http.MethodOptions, "/v1/images/generations", nil)
	request.Header.Set("Origin", "https://client.example.test")
	request.Header.Set("Access-Control-Request-Method", http.MethodPost)
	request.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want %d: %s", recorder.Code, http.StatusNoContent, recorder.Body.String())
	}
	if got := strings.ToLower(recorder.Header().Get("Access-Control-Allow-Headers")); !strings.Contains(got, "authorization") {
		t.Fatalf("Access-Control-Allow-Headers = %q, missing authorization", got)
	}
}

func TestAdministratorArtifactRoutesAreSessionProtected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := New(&config.Config{
		AppEnv:      "development",
		CORSOrigins: []string{"https://admin.example.test"},
	}, nil, nil, Handlers{
		Health:         (*handler.HealthHandler)(nil),
		V1:             (*handler.V1Handler)(nil),
		Auth:           (*handler.AuthHandler)(nil),
		APICredentials: (*handler.UserToolsHandler)(nil),
		Admin:          (*handler.AdminConsoleHandler)(nil),
		BannedWords:    (*handler.BannedWordsHandler)(nil),
	})

	want := map[string]bool{
		http.MethodPost + " /admin/api/test":                 false,
		http.MethodGet + " /admin/api/test/artifacts/:id":    false,
		http.MethodGet + " /admin/api/artifacts/:id/content": false,
		http.MethodPost + " /admin/api/accounts/delete-dead": false,
	}
	for _, route := range engine.Routes() {
		key := route.Method + " " + route.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for route, found := range want {
		if !found {
			t.Fatalf("administrator account test route %s is not registered", route)
		}
	}
}
