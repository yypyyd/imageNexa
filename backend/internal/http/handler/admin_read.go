package handler

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"backend/internal/model"
	"backend/internal/service"

	"github.com/gin-gonic/gin"
)

type AdminReadHandler struct {
	admin *service.AdminReadService
}

func NewAdminReadHandler(admin *service.AdminReadService) *AdminReadHandler {
	return &AdminReadHandler{admin: admin}
}

func (h *AdminReadHandler) Users(c *gin.Context) {
	users, stats, err := h.admin.Users(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": "failed to load users"})
		return
	}

	// Server-side filtering (role / status / 搜索 邮箱·名称·ID) so pagination stays
	// correct across pages. stats is computed over the full set (KPI strip).
	role := strings.TrimSpace(c.Query("role"))
	status := strings.TrimSpace(c.Query("status"))
	q := strings.ToLower(strings.TrimSpace(c.Query("q")))
	filtered := make([]model.User, 0, len(users))
	for _, user := range users {
		if role != "" && user.Role != role {
			continue
		}
		if status != "" && user.Status != status {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(user.Email), q) &&
			!strings.Contains(strings.ToLower(user.Name), q) &&
			!strings.Contains(strings.ToLower(user.ID), q) {
			continue
		}
		filtered = append(filtered, user)
	}

	total := len(filtered)
	limit, offset := pageParams(c, 20)
	page := pageSlice(filtered, limit, offset)

	out := make([]gin.H, 0, len(page))
	for _, user := range page {
		row := userPublic(user)
		row["generation_count"] = user.GenerationCount
		row["banned_word_hits"] = user.BannedWordHits
		out = append(out, row)
	}
	c.JSON(http.StatusOK, gin.H{"data": out, "total": total, "limit": limit, "offset": offset, "stats": stats})
}

func (h *AdminReadHandler) Models(c *gin.Context) {
	items, err := h.admin.ModelsView(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": "failed to load models"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": items})
}

func (h *AdminReadHandler) Stats(c *gin.Context) {
	stats, err := h.admin.Stats(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": "failed to load stats"})
		return
	}
	c.JSON(http.StatusOK, stats)
}

func (h *AdminReadHandler) Dashboard(c *gin.Context) {
	data, err := h.admin.Dashboard(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": "failed to load dashboard"})
		return
	}
	c.JSON(http.StatusOK, data)
}

func (h *AdminReadHandler) Invites(c *gin.Context) {
	items, stats, err := h.admin.Invites(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": "failed to load invites"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "stats": stats})
}

// DeleteImage removes one generated file (plus derived stills) and blanks the
// log rows referencing it. Admin 图片管理 delete; ?name= is the storage key.
func (h *AdminReadHandler) DeleteImage(c *gin.Context) {
	if err := h.admin.DeleteFile(c.Request.Context(), c.Query("name")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *AdminReadHandler) Providers(c *gin.Context) {
	items, err := h.admin.Providers(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": "failed to load providers"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": items})
}

func (h *AdminReadHandler) Images(c *gin.Context) {
	limit := parseInt(c.Query("limit"), 30)
	offset := parseInt(c.Query("offset"), 0)
	kind := c.Query("kind")
	items, total, stats, err := h.admin.Images(c.Request.Context(), limit, offset, kind)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": "failed to load images"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"data":   items,
		"total":  total,
		"limit":  limit,
		"offset": offset,
		"stats":  stats,
	})
}

func userPublic(user model.User) gin.H {
	keys := make([]gin.H, 0, len(user.APIKeys))
	for _, key := range user.APIKeys {
		keys = append(keys, gin.H{
			"id":           key.ID,
			"name":         key.Name,
			"key_preview":  key.KeyPreview,
			"created_at":   unixSec(key.CreatedAt),
			"last_used_at": unixSecPtr(key.LastUsedAt),
		})
	}
	return gin.H{
		"id":                   user.ID,
		"email":                user.Email,
		"name":                 user.Name,
		"role":                 user.Role,
		"status":               user.Status,
		"credits":              user.Credits,
		"notes":                user.Notes,
		"recharge_total":       user.RechargeTotal,
		"concurrency_group_id": user.ConcurrencyGroupID,
		"created_at":           unixSec(user.CreatedAt),
		"last_login_at":        unixSecPtr(user.LastLoginAt),
		"last_login_ip":        user.LastLoginIP,
		"invite_code":          user.InviteCode,
		"invited_by":           user.InvitedBy,
		"checkin_last":         user.CheckinLast,
		"checkin_streak":       user.CheckinStreak,
		"api_keys":             keys,
		"has_password":         user.PasswordHash != "",
	}
}

// unixSec / unixSecPtr render timestamps as unix SECONDS — the frontend's
// fmtTs/fmtRelative expect seconds (matching the Python reference's time.time()),
// not the RFC3339 string Go marshals a time.Time into (which parses to NaN → "—").
func unixSec(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

func unixSecPtr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.Unix()
}

func parseInt(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	if n, err := strconv.Atoi(raw); err == nil {
		return n
	}
	return fallback
}

func emptyStringNil(v string) any {
	if v == "" {
		return nil
	}
	return v
}
