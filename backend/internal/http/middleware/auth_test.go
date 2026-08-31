package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"backend/internal/config"
	"github.com/gin-gonic/gin"
)

func TestOriginAllowedUsesConfiguredOriginsAndSameHost(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name   string
		origin string
		host   string
		want   bool
	}{
		{name: "configured", origin: "https://admin.example.com", host: "api.example.com", want: true},
		{name: "same host", origin: "https://api.example.com", host: "api.example.com", want: true},
		{name: "untrusted", origin: "https://evil.example", host: "api.example.com", want: false},
		{name: "missing", host: "api.example.com", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			context, _ := gin.CreateTestContext(httptest.NewRecorder())
			context.Request = httptest.NewRequest(http.MethodPost, "https://"+test.host+"/admin/api/test", nil)
			context.Request.Host = test.host
			context.Request.Header.Set("Origin", test.origin)
			cfg := &config.Config{CORSOrigins: []string{"https://admin.example.com"}}
			if got := originAllowed(context, cfg); got != test.want {
				t.Fatalf("originAllowed() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestUnsafeMethods(t *testing.T) {
	if isUnsafeMethod(http.MethodGet) || isUnsafeMethod(http.MethodHead) || isUnsafeMethod(http.MethodOptions) {
		t.Fatal("safe HTTP methods were classified as unsafe")
	}
	if !isUnsafeMethod(http.MethodPost) || !isUnsafeMethod(http.MethodDelete) {
		t.Fatal("mutating HTTP methods were classified as safe")
	}
}
