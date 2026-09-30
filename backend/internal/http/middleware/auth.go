package middleware

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"backend/internal/config"
	"backend/internal/model"
	"backend/internal/service"
	"github.com/gin-gonic/gin"
)

const (
	currentAdminKey         = "current_admin"
	currentSessionKey       = "current_session"
	currentAPICredentialKey = "api_credential"
	CSRFHeaderName          = "X-CSRF-Token"
)

func RequireAdminSession(auth *service.AuthService, cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		cookieToken := readCookie(c, cfg.SessionCookieName)
		admin, session, err := auth.CurrentAdminFromCookie(c.Request.Context(), cookieToken)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"detail": "failed to validate session"})
			c.Abort()
			return
		}
		if admin == nil || session == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"detail": "未登录或会话已过期"})
			c.Abort()
			return
		}
		if isUnsafeMethod(c.Request.Method) {
			if !originAllowed(c, cfg) {
				c.JSON(http.StatusForbidden, gin.H{"detail": "请求来源不受信任"})
				c.Abort()
				return
			}
			provided := strings.TrimSpace(c.GetHeader(CSRFHeaderName))
			if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(session.CSRFToken)) != 1 {
				c.JSON(http.StatusForbidden, gin.H{"detail": "CSRF token 无效"})
				c.Abort()
				return
			}
		}

		refreshSessionCookie(c, cfg, cookieToken)
		c.Set(currentAdminKey, admin)
		c.Set(currentSessionKey, session)
		c.Next()
	}
}

// RequireTrustedOrigin protects unauthenticated login/initialization writes
// from cross-site form submissions. Authenticated writes additionally require
// the per-session CSRF token in RequireAdminSession.
func RequireTrustedOrigin(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if isUnsafeMethod(c.Request.Method) && !originAllowed(c, cfg) {
			c.JSON(http.StatusForbidden, gin.H{"detail": "请求来源不受信任"})
			c.Abort()
			return
		}
		c.Next()
	}
}

func RequireAPICredential(credentials *service.APICredentialService) gin.HandlerFunc {
	return func(c *gin.Context) {
		credential, err := credentials.AuthenticateBearer(c.Request.Context(), c.GetHeader("Authorization"))
		if err != nil {
			if errors.Is(err, service.ErrInvalidAPICredential) {
				c.Header("WWW-Authenticate", `Bearer realm="nexa"`)
				writeOpenAIAuthError(c, http.StatusUnauthorized, "Incorrect API key provided.", "invalid_api_key")
			} else {
				writeOpenAIAuthError(c, http.StatusInternalServerError, "Authentication service unavailable.", "internal_error")
			}
			c.Abort()
			return
		}
		c.Set(currentAPICredentialKey, credential)
		c.Next()
	}
}

func CurrentAPICredential(c *gin.Context) *model.APICredential {
	value, ok := c.Get(currentAPICredentialKey)
	if !ok {
		return nil
	}
	credential, _ := value.(*model.APICredential)
	return credential
}

func isUnsafeMethod(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

func originAllowed(c *gin.Context, cfg *config.Config) bool {
	origin := normalizeOrigin(c.GetHeader("Origin"))
	if origin == "" || origin == "null" {
		return false
	}
	for _, allowed := range cfg.CORSOrigins {
		if origin == normalizeOrigin(allowed) {
			return true
		}
	}

	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, strings.TrimSpace(c.Request.Host))
}

func normalizeOrigin(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return ""
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	return strings.ToLower(parsed.Scheme + "://" + parsed.Host)
}

func refreshSessionCookie(c *gin.Context, cfg *config.Config, cookieToken string) {
	if cookieToken == "" {
		return
	}
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(cfg.SessionCookieName, cookieToken, int(cfg.SessionTTL.Seconds()), "/", "", cfg.CookieSecure, true)
}

func readCookie(c *gin.Context, name string) string {
	value, err := c.Cookie(name)
	if err != nil {
		return ""
	}
	return value
}

func writeOpenAIAuthError(c *gin.Context, status int, message, code string) {
	c.JSON(status, gin.H{
		"error": gin.H{
			"message": message,
			"type":    "invalid_request_error",
			"param":   nil,
			"code":    code,
		},
	})
}
