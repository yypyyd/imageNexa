package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"backend/internal/config"
	"backend/internal/model"
	"backend/internal/service"
	"github.com/gin-gonic/gin"
)

func TestWriteSessionIsCookieOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/admin/api/auth/login", nil)
	handler := &AuthHandler{cfg: &config.Config{
		SessionCookieName: "admin_session",
		SessionTTL:        time.Hour,
		CookieSecure:      true,
	}}
	session := &service.SessionPayload{CSRFToken: "csrf-value", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	handler.writeSession(context, "server-session-secret", session, &model.Admin{
		ID: model.AdminSingletonID, Username: "admin", Status: model.AdminStatusActive,
	})

	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if _, exists := body["token"]; exists || strings.Contains(recorder.Body.String(), "server-session-secret") {
		t.Fatalf("login response exposed session token: %s", recorder.Body.String())
	}
	if body["csrf_token"] != "csrf-value" {
		t.Fatalf("login response omitted CSRF token: %#v", body)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie flags are unsafe: %#v", cookies)
	}
}

func TestClientIPSpoofedFirstHopCannotOverrideDirectClient(t *testing.T) {
	context := newIPTestContext("127.0.0.1:43110", "203.0.113.99, 198.51.100.42")
	got := clientIP(context, []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})
	if got != "198.51.100.42" {
		t.Fatalf("clientIP() = %q, want direct client 198.51.100.42", got)
	}
}

func TestClientIPIgnoresForwardedHeaderFromUntrustedPeer(t *testing.T) {
	context := newIPTestContext("198.51.100.42:43110", "203.0.113.99")
	got := clientIP(context, []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})
	if got != "198.51.100.42" {
		t.Fatalf("clientIP() = %q, want socket peer 198.51.100.42", got)
	}
}

func TestClientIPWalksTrustedProxyChainFromRight(t *testing.T) {
	context := newIPTestContext("127.0.0.1:43110", "203.0.113.7, 10.20.30.40")
	trusted := []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("10.0.0.0/8"),
	}
	if got := clientIP(context, trusted); got != "203.0.113.7" {
		t.Fatalf("clientIP() = %q, want original untrusted client", got)
	}
}

func TestClientIPMalformedForwardedChainFallsBackToSocketPeer(t *testing.T) {
	context := newIPTestContext("127.0.0.1:43110", "203.0.113.99, not-an-ip")
	got := clientIP(context, []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})
	if got != "127.0.0.1" {
		t.Fatalf("clientIP() = %q, want trusted socket peer fallback", got)
	}
}

func TestBootstrapTokenFailureDoesNotReflectSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/admin/api/auth/initialize", nil)
	context.Request.Header.Set(AdminBootstrapTokenHeader, "provided-secret")
	handler := &AuthHandler{cfg: &config.Config{AdminBootstrapToken: "expected-secret"}}

	if handler.requireBootstrapToken(context) {
		t.Fatal("invalid bootstrap token was accepted")
	}
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
	for _, secret := range []string{"provided-secret", "expected-secret"} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatalf("bootstrap response exposed %q: %s", secret, recorder.Body.String())
		}
	}
}

func TestBootstrapTokenUsesFixedHeaderValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/admin/api/auth/initialize", nil)
	context.Request.Header.Set(AdminBootstrapTokenHeader, "expected-secret")
	handler := &AuthHandler{cfg: &config.Config{AdminBootstrapToken: "expected-secret"}}

	if !handler.requireBootstrapToken(context) {
		t.Fatalf("matching bootstrap header was rejected: %s", recorder.Body.String())
	}
}

func TestBootstrapTokenCannotBeEmptyOnDevelopmentEndpoint(t *testing.T) {
	if validBootstrapToken("", "") {
		t.Fatal("empty bootstrap token was accepted")
	}
}

func TestAuthServiceErrorDoesNotReflectInternalDetails(t *testing.T) {
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	writeAuthServiceError(context, errors.New("redis failed Authorization: Bearer secret"))
	if recorder.Code != http.StatusInternalServerError || strings.Contains(recorder.Body.String(), "Bearer") || strings.Contains(recorder.Body.String(), "redis") {
		t.Fatalf("unsafe auth error response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestAuthServiceErrorAllowsExplicitValidation(t *testing.T) {
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	writeAuthServiceError(context, errors.New("密码长度需为 12 到 64 个字符且不能超过 72 字节"))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "密码长度需为 12 到 64 个字符且不能超过 72 字节") {
		t.Fatalf("validation response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func newIPTestContext(remoteAddress, forwardedFor string) *gin.Context {
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/admin/api/auth/login", nil)
	context.Request.RemoteAddr = remoteAddress
	context.Request.Header.Set("X-Forwarded-For", forwardedFor)
	return context
}
