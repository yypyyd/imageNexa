package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"backend/internal/model"
	"backend/internal/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// UserToolsHandler owns the administrator-managed service API credentials.
type UserToolsHandler struct {
	credentials *service.APICredentialService
}

func NewUserToolsHandler(credentials *service.APICredentialService) *UserToolsHandler {
	return &UserToolsHandler{credentials: credentials}
}

func (h *UserToolsHandler) APICredentialsList(c *gin.Context) {
	includeRevoked, _ := strconv.ParseBool(c.DefaultQuery("include_revoked", "false"))
	items, err := h.credentials.List(c.Request.Context(), includeRevoked)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": "failed to load API Keys"})
		return
	}
	out := make([]gin.H, 0, len(items))
	for _, item := range items {
		out = append(out, gin.H{
			"id": item.ID, "name": item.Name, "key_preview": item.KeyPreview,
			"status": item.Status, "enabled": item.Status == model.APICredentialStatusActive,
			"concurrency_limit": item.ConcurrencyLimit,
			"active_requests":   h.credentials.ActiveRequests(c.Request.Context(), item.ID),
			"last_used_at":      item.LastUsedAt, "revoked_at": item.RevokedAt,
			"created_at": item.CreatedAt, "updated_at": item.UpdatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": out, "total": len(out)})
}

func (h *UserToolsHandler) APICredentialsCreate(c *gin.Context) {
	var input service.CreateAPICredentialInput
	if !bindAdminJSON(c, &input, "invalid request body") {
		return
	}
	created, err := h.credentials.Create(c.Request.Context(), input)
	if err != nil {
		writeCredentialError(c, err)
		return
	}
	// key is intentionally present only in this one response. Every later API
	// response serializes APICredential with KeyHash excluded.
	c.JSON(http.StatusCreated, gin.H{
		"ok":         true,
		"key":        created.Key,
		"credential": created.Credential,
	})
}

func (h *UserToolsHandler) APICredentialsUpdate(c *gin.Context) {
	var input service.UpdateAPICredentialInput
	if !bindAdminJSON(c, &input, "invalid request body") {
		return
	}
	credential, err := h.credentials.Update(c.Request.Context(), c.Param("id"), input)
	if err != nil {
		writeCredentialError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "credential": credential})
}

func (h *UserToolsHandler) APICredentialsRevoke(c *gin.Context) {
	if err := h.credentials.Revoke(c.Request.Context(), c.Param("id")); err != nil {
		writeCredentialError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *UserToolsHandler) APICredentialsRotate(c *gin.Context) {
	created, err := h.credentials.Rotate(c.Request.Context(), c.Param("id"))
	if err != nil {
		writeCredentialError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"ok": true, "secret": created.Key, "key": created.Key, "credential": created.Credential,
	})
}

func writeCredentialError(c *gin.Context, err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"detail": "API Key 不存在"})
		return
	}
	if errors.Is(err, service.ErrCredentialServiceUnavailable) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"detail": "API Key service is temporarily unavailable"})
		return
	}
	message := strings.TrimSpace(err.Error())
	for _, safe := range []string{
		"API Key 名称不能为空",
		"API Key 名称不能超过 100 个字符",
		"API Key 名称需为 1 到 100 个字符",
		"并发上限不能小于 0",
		"已吊销的 API Key 不能修改",
		"请使用吊销接口永久吊销 API Key",
		"API Key 状态不正确",
	} {
		if message == safe {
			c.JSON(http.StatusBadRequest, gin.H{"detail": message})
			return
		}
	}
	c.JSON(http.StatusInternalServerError, gin.H{"detail": "API Key operation failed"})
}
