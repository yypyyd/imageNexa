package handler

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"backend/internal/config"
	"backend/internal/model"
	"backend/internal/service"
	"github.com/gin-gonic/gin"
)

type AuthHandler struct {
	cfg     *config.Config
	auth    *service.AuthService
	limiter *service.RateLimitService
}

const AdminBootstrapTokenHeader = "X-Admin-Bootstrap-Token"

func NewAuthHandler(cfg *config.Config, auth *service.AuthService, limiter *service.RateLimitService) *AuthHandler {
	return &AuthHandler{cfg: cfg, auth: auth, limiter: limiter}
}

func (h *AuthHandler) Config(c *gin.Context) {
	data, err := h.auth.AuthConfig(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": "failed to load auth config"})
		return
	}
	initialized, _ := data["initialized"].(bool)
	data["bootstrap_token_required"] = !initialized
	c.JSON(http.StatusOK, data)
}

// Initialize creates the singleton administrator. The repository serializes
// concurrent attempts, so this endpoint permanently closes after one success.
func (h *AuthHandler) Initialize(c *gin.Context) {
	initialized, err := h.auth.Initialized(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": "failed to load initialization status"})
		return
	}
	if initialized {
		c.JSON(http.StatusConflict, gin.H{"detail": "超级管理员已初始化"})
		return
	}
	if !h.requireBootstrapToken(c) {
		return
	}
	ip := clientIP(c, h.cfg.TrustedProxyCIDRs)
	if err := h.enforceRateLimit(c, "auth:initialize:ip:"+ip, 5, time.Hour); err != nil {
		return
	}

	var body struct {
		Username string `json:"username"`
		Name     string `json:"name"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !bindAdminJSON(c, &body, "invalid request body") {
		return
	}
	username := strings.TrimSpace(body.Username)
	if username == "" {
		username = strings.TrimSpace(body.Name)
	}
	admin, token, session, err := h.auth.Initialize(
		c.Request.Context(), username, body.Email, body.Password, ip,
	)
	if err != nil {
		if errors.Is(err, service.ErrAdminAlreadyInitialized) {
			c.JSON(http.StatusConflict, gin.H{"detail": "超级管理员已初始化"})
			return
		}
		writeAuthServiceError(c, err)
		return
	}
	h.writeSession(c, token, session, admin)
}

func (h *AuthHandler) Login(c *gin.Context) {
	var body struct {
		Identifier string `json:"identifier"`
		Email      string `json:"email"`
		Username   string `json:"username"`
		Password   string `json:"password"`
	}
	if !bindAdminJSON(c, &body, "invalid request body") {
		return
	}
	identifier := strings.TrimSpace(body.Identifier)
	if identifier == "" {
		identifier = strings.TrimSpace(body.Email)
	}
	if identifier == "" {
		identifier = strings.TrimSpace(body.Username)
	}
	if identifier == "" || body.Password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "账号或密码不能为空"})
		return
	}
	ip := clientIP(c, h.cfg.TrustedProxyCIDRs)
	if err := h.enforceRateLimit(c, "auth:login:ip:"+ip, 20, 15*time.Minute); err != nil {
		return
	}
	admin, token, session, err := h.auth.Login(c.Request.Context(), identifier, body.Password, ip)
	if err != nil {
		if writeLoginLocked(c, err) {
			return
		}
		if errors.Is(err, service.ErrAuthFailed) {
			c.JSON(http.StatusUnauthorized, gin.H{"detail": "账号或密码错误"})
			return
		}
		writeAuthServiceError(c, err)
		return
	}
	h.writeSession(c, token, session, admin)
}

func (h *AuthHandler) Logout(c *gin.Context) {
	token := readAdminCookie(c, h.cfg.SessionCookieName)
	_ = h.auth.Logout(c.Request.Context(), token)
	h.clearSessionCookie(c)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *AuthHandler) Me(c *gin.Context) {
	admin := currentAdmin(c)
	session := currentSession(c)
	if admin == nil || session == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"detail": "未登录或会话已过期"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":         true,
		"expires_at": session.ExpiresAt,
		"csrf_token": session.CSRFToken,
		"admin":      service.PublicAdmin(admin),
	})
}

func (h *AuthHandler) ChangePassword(c *gin.Context) {
	admin := currentAdmin(c)
	if admin == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"detail": "未登录或会话已过期"})
		return
	}
	var body struct {
		CurrentPassword string `json:"current_password"`
		Current         string `json:"current"`
		NewPassword     string `json:"new_password"`
		Password        string `json:"password"`
	}
	if !bindAdminJSON(c, &body, "invalid request body") {
		return
	}
	currentPassword := body.CurrentPassword
	if currentPassword == "" {
		currentPassword = body.Current
	}
	newPassword := body.NewPassword
	if newPassword == "" {
		newPassword = body.Password
	}
	if err := h.auth.ChangePassword(c.Request.Context(), admin.ID, currentPassword, newPassword); err != nil {
		writeAuthServiceError(c, err)
		return
	}
	// Password changes invalidate all sessions by version; also eagerly remove
	// the current Redis entry and cookie so the browser cannot look logged in.
	_ = h.auth.Logout(c.Request.Context(), readAdminCookie(c, h.cfg.SessionCookieName))
	h.clearSessionCookie(c)
	c.JSON(http.StatusOK, gin.H{"ok": true, "reauthenticate": true})
}

func writeAuthServiceError(c *gin.Context, err error) {
	if errors.Is(err, service.ErrAuthServiceUnavailable) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"detail": "administrator authentication is temporarily unavailable"})
		return
	}
	if message, ok := safeAuthValidationMessage(err); ok {
		c.JSON(http.StatusBadRequest, gin.H{"detail": message})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"detail": "authentication operation failed"})
}

func safeAuthValidationMessage(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	message := strings.TrimSpace(err.Error())
	for _, safe := range []string{
		"邮箱不能为空", "邮箱长度不能超过 254 个字符", "邮箱格式不正确",
		"用户名不能为空", "用户名长度需为 6 到 24 个字符", "用户名长度不能超过 24 个字符", "用户名只能使用字母和数字",
		"密码不能为空", "密码长度需为 12 到 64 个字符且不能超过 72 字节", "密码不能包含空白字符", "密码包含不允许的字符", "密码必须同时包含大写字母、小写字母、数字和符号",
		"账号不能为空", "当前密码不能为空", "当前密码错误",
	} {
		if message == safe {
			return message, true
		}
	}
	return "", false
}

func currentAdmin(c *gin.Context) *model.Admin {
	value, ok := c.Get("current_admin")
	if !ok {
		return nil
	}
	admin, _ := value.(*model.Admin)
	return admin
}

func currentSession(c *gin.Context) *service.SessionPayload {
	value, ok := c.Get("current_session")
	if !ok {
		return nil
	}
	session, _ := value.(*service.SessionPayload)
	return session
}

func (h *AuthHandler) writeSession(c *gin.Context, token string, session *service.SessionPayload, admin *model.Admin) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(h.cfg.SessionCookieName, token, int(h.cfg.SessionTTL.Seconds()), "/", "", h.cfg.CookieSecure, true)
	c.JSON(http.StatusOK, gin.H{
		"ok":         true,
		"expires_at": session.ExpiresAt,
		"csrf_token": session.CSRFToken,
		"admin":      service.PublicAdmin(admin),
	})
}

func (h *AuthHandler) clearSessionCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(h.cfg.SessionCookieName, "", -1, "/", "", h.cfg.CookieSecure, true)
}

func writeLoginLocked(c *gin.Context, err error) bool {
	var locked *service.LoginLockedError
	if errors.As(err, &locked) {
		c.Header("Retry-After", strconv.Itoa(locked.RetryAfter))
		c.JSON(http.StatusTooManyRequests, gin.H{"detail": locked.Error()})
		return true
	}
	return false
}

func clientIP(c *gin.Context, trustedProxies []netip.Prefix) string {
	peer, ok := parseIPAddress(c.Request.RemoteAddr)
	if !ok {
		return "unknown"
	}
	if !addressInPrefixes(peer, trustedProxies) {
		return peer.String()
	}

	// X-Forwarded-For is ordered from the original client to the newest proxy.
	// Walk it backwards and discard only explicitly trusted proxy hops. The first
	// untrusted address is the rate-limit identity; client-supplied values farther
	// left can therefore never override the peer that actually reached our edge.
	forwarded := strings.Split(c.GetHeader("X-Forwarded-For"), ",")
	for index := len(forwarded) - 1; index >= 0; index-- {
		address, valid := parseIPAddress(forwarded[index])
		if !valid {
			// A malformed hop makes everything to its left unauthenticated data.
			// Fall back to the socket peer instead of skipping across the gap.
			return peer.String()
		}
		if !addressInPrefixes(address, trustedProxies) {
			return address.String()
		}
	}
	return peer.String()
}

func parseIPAddress(value string) (netip.Addr, bool) {
	value = strings.TrimSpace(value)
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	} else {
		value = strings.Trim(value, "[]")
	}
	address, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Addr{}, false
	}
	return address.Unmap(), true
}

func addressInPrefixes(address netip.Addr, prefixes []netip.Prefix) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func (h *AuthHandler) requireBootstrapToken(c *gin.Context) bool {
	if validBootstrapToken(h.cfg.AdminBootstrapToken, c.GetHeader(AdminBootstrapTokenHeader)) {
		return true
	}
	// Use one response for a missing, invalid, or locally unconfigured token and
	// never reflect either value. Production cannot reach this state without a
	// configured token because config validation fails during process startup.
	c.JSON(http.StatusForbidden, gin.H{"detail": "初始化凭据无效"})
	return false
}

func validBootstrapToken(expected, provided string) bool {
	expected = strings.TrimSpace(expected)
	provided = strings.TrimSpace(provided)
	expectedHash := sha256.Sum256([]byte(expected))
	providedHash := sha256.Sum256([]byte(provided))
	equal := subtle.ConstantTimeCompare(expectedHash[:], providedHash[:])
	return expected != "" && provided != "" && equal == 1
}

func (h *AuthHandler) enforceRateLimit(c *gin.Context, bucket string, limit int64, window time.Duration) error {
	if h.limiter == nil {
		return nil
	}
	if err := h.limiter.Enforce(c.Request.Context(), bucket, limit, window); err != nil {
		if errors.Is(err, service.ErrRateLimited) {
			c.JSON(http.StatusTooManyRequests, gin.H{"detail": err.Error()})
			return err
		}
		c.JSON(http.StatusInternalServerError, gin.H{"detail": "rate limiter unavailable"})
		return err
	}
	return nil
}

func readAdminCookie(c *gin.Context, name string) string {
	value, err := c.Cookie(name)
	if err != nil {
		return ""
	}
	return value
}
