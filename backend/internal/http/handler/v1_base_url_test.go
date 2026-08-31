package handler

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequestBaseURLIgnoresUntrustedForwardedHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "http://internal/v1/images/generations", nil)
	c.Request.Host = "media.example.test"
	c.Request.Header.Set("X-Forwarded-Proto", "https")
	c.Request.Header.Set("X-Forwarded-Port", "9445")

	if got := requestBaseURL(c); got != "http://media.example.test" {
		t.Fatalf("requestBaseURL() = %q", got)
	}
}

func TestRequestBaseURLUsesValidatedDirectHost(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "http://internal/v1/images/generations", nil)
	c.Request.Host = "example.test:8443"
	c.Request.Header.Set("X-Forwarded-Proto", "https")
	c.Request.Header.Set("X-Forwarded-Port", "9445")

	if got := requestBaseURL(c); got != "http://example.test:8443" {
		t.Fatalf("requestBaseURL() = %q", got)
	}
}

func TestRequestBaseURLRejectsMalformedHostAndPort(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, host := range []string{"example.test@evil.test", "example.test/path", "example.test:99999"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("GET", "http://internal/v1/models", nil)
		c.Request.Host = host
		if got := requestBaseURL(c); got != "" {
			t.Fatalf("requestBaseURL() for %q = %q, want empty", host, got)
		}
	}
}

func TestImageAsyncRequestedIsOptIn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name   string
		url    string
		prefer string
		want   bool
	}{
		{name: "default stays synchronous", url: "/v1/images/generations"},
		{name: "prefer header", url: "/v1/images/generations", prefer: "respond-async", want: true},
		{name: "prefer with parameter", url: "/v1/images/generations", prefer: "respond-async; wait=5", want: true},
		{name: "query flag", url: "/v1/images/generations?async=true", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", tt.url, nil)
			c.Request.Header.Set("Prefer", tt.prefer)
			if got := imageAsyncRequested(c); got != tt.want {
				t.Fatalf("imageAsyncRequested() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnsureImageRequestIDUsesExistingValues(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/v1/images/generations", nil)
	c.Writer.Header().Set("X-Request-Id", "middleware-id")

	if got := ensureImageRequestID(c, "explicit-id"); got != "explicit-id" {
		t.Fatalf("explicit request id = %q", got)
	}
	if got := ensureImageRequestID(c, ""); got != "middleware-id" {
		t.Fatalf("middleware request id = %q", got)
	}
}
