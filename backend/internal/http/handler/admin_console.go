package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"backend/internal/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type AdminConsoleHandler struct {
	console *service.AdminConsoleService
}

func NewAdminConsoleHandler(console *service.AdminConsoleService) *AdminConsoleHandler {
	return &AdminConsoleHandler{console: console}
}

func (h *AdminConsoleHandler) LogicalModels(c *gin.Context) {
	items, err := h.console.ListLogicalModels(c.Request.Context())
	if err != nil {
		adminConsoleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "total": len(items)})
}

func (h *AdminConsoleHandler) UpdateLogicalModel(c *gin.Context) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !bindAdminJSON(c, &body, "enabled is required") {
		return
	}
	if body.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "enabled is required"})
		return
	}
	item, err := h.console.UpdateLogicalModel(c.Request.Context(), c.Param("id"), *body.Enabled)
	if err != nil {
		adminConsoleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": item})
}

func (h *AdminConsoleHandler) UpdateLogicalRoute(c *gin.Context) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !bindAdminJSON(c, &body, "enabled is required") {
		return
	}
	if body.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "enabled is required"})
		return
	}
	item, err := h.console.UpdateLogicalRoute(c.Request.Context(), c.Param("id"), c.Param("route_id"), *body.Enabled)
	if err != nil {
		adminConsoleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": item})
}

func (h *AdminConsoleHandler) Accounts(c *gin.Context) {
	limit, offset, page := adminPage(c)
	items, total, err := h.console.ListAccounts(c.Request.Context(), service.AccountListFilter{
		Query: c.Query("q"), Provider: c.Query("provider"), Status: c.Query("status"), ModelID: c.Query("model_id"), Limit: limit, Offset: offset,
	})
	if err != nil {
		adminConsoleError(c, err)
		return
	}
	providerCounts, err := h.console.CountAccountsByProvider(c.Request.Context())
	if err != nil {
		adminConsoleError(c, err)
		return
	}
	providerHealth, err := h.console.CountAccountHealthByProvider(c.Request.Context())
	if err != nil {
		adminConsoleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "total": total, "page": page, "limit": limit, "provider_counts": providerCounts, "provider_health": providerHealth})
}

func (h *AdminConsoleHandler) Overview(c *gin.Context) {
	item, err := h.console.Overview(c.Request.Context())
	if err != nil {
		adminConsoleError(c, err)
		return
	}
	c.JSON(http.StatusOK, item)
}

func (h *AdminConsoleHandler) ImportAccount(c *gin.Context) {
	var body service.AccountImportInput
	if !bindAdminJSON(c, &body, "invalid request body") {
		return
	}
	item, err := h.console.ImportAccount(c.Request.Context(), body)
	if err != nil {
		adminConsoleError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": item})
}

func (h *AdminConsoleHandler) UpdateAccount(c *gin.Context) {
	var body map[string]any
	if !bindAdminJSON(c, &body, "invalid request body") {
		return
	}
	item, err := h.console.UpdateAccount(c.Request.Context(), c.Param("id"), body)
	if err != nil {
		adminConsoleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": item})
}

func (h *AdminConsoleHandler) DeleteAccount(c *gin.Context) {
	if err := h.console.DeleteAccount(c.Request.Context(), c.Param("id")); err != nil {
		adminConsoleError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *AdminConsoleHandler) DeleteDeadAccounts(c *gin.Context) {
	deleted, err := h.console.DeleteDeadAccounts(c.Request.Context())
	if err != nil {
		adminConsoleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": deleted})
}

func (h *AdminConsoleHandler) SetAccountRoute(c *gin.Context) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !bindAdminJSON(c, &body, "enabled is required") {
		return
	}
	if body.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "enabled is required"})
		return
	}
	if err := h.console.SetAccountRoute(c.Request.Context(), c.Param("id"), c.Param("binding_id"), *body.Enabled); err != nil {
		adminConsoleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *AdminConsoleHandler) RefreshAccountQuota(c *gin.Context) {
	item, err := h.console.RefreshAccountQuota(c.Request.Context(), c.Param("id"))
	if err != nil {
		adminConsoleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": item})
}

func (h *AdminConsoleHandler) Logs(c *gin.Context) {
	h.listEvents(c, false)
}

func (h *AdminConsoleHandler) Artifacts(c *gin.Context) {
	h.listEvents(c, true)
}

func (h *AdminConsoleHandler) listEvents(c *gin.Context, artifacts bool) {
	limit, offset, page := adminPage(c)
	items, total, err := h.console.ListLogs(c.Request.Context(), service.LogListFilter{
		CredentialID: c.Query("credential_id"), Model: c.Query("model"), Kind: c.Query("kind"),
		Status: c.Query("status"), Source: c.Query("source"), Query: c.Query("q"), Artifacts: artifacts, Limit: limit, Offset: offset,
	})
	if err != nil {
		adminConsoleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "total": total, "page": page, "limit": limit})
}

func (h *AdminConsoleHandler) Settings(c *gin.Context) {
	item, err := h.console.GetSettings(c.Request.Context())
	if err != nil {
		adminConsoleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": item})
}

func (h *AdminConsoleHandler) UpdateSettings(c *gin.Context) {
	var body map[string]any
	if !bindAdminJSON(c, &body, "invalid request body") {
		return
	}
	item, err := h.console.UpdateSettings(c.Request.Context(), body)
	if err != nil {
		adminConsoleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": item})
}

func adminPage(c *gin.Context) (limit, offset, page int) {
	limit, _ = strconv.Atoi(strings.TrimSpace(c.DefaultQuery("limit", "50")))
	page, _ = strconv.Atoi(strings.TrimSpace(c.DefaultQuery("page", "1")))
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if page <= 0 {
		page = 1
	}
	offset = (page - 1) * limit
	return
}

func adminConsoleError(c *gin.Context, err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, service.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"detail": "resource not found"})
		return
	}
	if message, ok := safeAdminValidationMessage(err); ok {
		c.JSON(http.StatusBadRequest, gin.H{"detail": message})
		return
	}
	// Provider errors and infrastructure errors can contain response bodies,
	// bearer tokens, cookies, proxy URLs, or database details. Only the explicit
	// validation messages above are safe to reflect to the administrator.
	c.JSON(http.StatusInternalServerError, gin.H{"detail": "administrator operation failed"})
}

func safeAdminValidationMessage(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	message := strings.TrimSpace(err.Error())
	for _, safe := range []string{
		"unsupported provider",
		"custom accounts must bind at least one existing canonical model",
		"status must be active or disabled",
		"weight must be between -1000 and 1000",
		"max_concurrency must be between 0 and 1000",
		"outbound_proxy must be an http(s) or socks5 URL",
		"outbound_proxy must be an absolute http(s) or socks5 URL",
		"outbound_proxy must not contain a query or fragment",
		"provider_proxies must be an object of pool => URL",
		"provider_extract_apis must be an object of pool => URL",
		"dola_session_api must be an absolute http(s) URL",
		"outbound_extract_api must be an absolute http(s) URL",
		"public_base_url must be an http(s) URL",
		"cookie required",
		"not a byteplus lumina cookie",
		"access_token required",
		"not a runway token",
		"sso token required",
		"not a grok sso token",
		"this token is grok, not chatgpt",
		"grok 需要网站 sso Cookie，不能导入 Sub2API/CPA 的 OAuth access_token",
		"not an OreateAI cookie",
		"base_url required",
		"base_url and key required",
	} {
		if message == safe {
			return message, true
		}
	}
	for _, setting := range []string{"logs_retention_days", "artifacts_retention_days"} {
		if message == setting+" must be between 1 and 3650" {
			return message, true
		}
	}
	if strings.HasPrefix(message, "provider_proxies.") ||
		strings.HasPrefix(message, "provider_proxies contains unknown provider") ||
		strings.HasPrefix(message, "provider_extract_apis.") ||
		strings.HasPrefix(message, "provider_extract_apis contains unknown provider") ||
		strings.HasPrefix(message, "outbound_extract_api ") ||
		strings.HasPrefix(message, "dola_session_api ") {
		return message, true
	}
	if strings.HasPrefix(message, "invalid base_url: public HTTPS on port 443 is required") ||
		strings.HasPrefix(message, "invalid base_url: query and fragment are not allowed") {
		return message, true
	}
	return "", false
}
