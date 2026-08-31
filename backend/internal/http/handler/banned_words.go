package handler

import (
	"net/http"
	"strings"

	"backend/internal/repo"
	"github.com/gin-gonic/gin"
)

// BannedWordsHandler — admin 违禁词管理: list / add / delete prompt blocklist
// entries. The generation path (V1Service.checkBannedPrompt) enforces them.
type BannedWordsHandler struct {
	words *repo.BannedWordRepository
}

func NewBannedWordsHandler(words *repo.BannedWordRepository) *BannedWordsHandler {
	return &BannedWordsHandler{words: words}
}

func (h *BannedWordsHandler) List(c *gin.Context) {
	items, err := h.words.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": "failed to load banned words"})
		return
	}
	// Server-side 搜索(违禁词) + pagination.
	q := strings.ToLower(strings.TrimSpace(c.Query("q")))
	if q != "" {
		kept := items[:0]
		for _, w := range items {
			if strings.Contains(strings.ToLower(w.Word), q) {
				kept = append(kept, w)
			}
		}
		items = kept
	}
	total := len(items)
	limit, offset := pageParams(c, 20)
	page := pageSlice(items, limit, offset)
	out := make([]gin.H, 0, len(page))
	for _, w := range page {
		out = append(out, gin.H{
			"id":         w.ID,
			"word":       w.Word,
			"hits":       w.Hits,
			"created_at": w.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": out, "total": total, "limit": limit, "offset": offset})
}

func (h *BannedWordsHandler) Create(c *gin.Context) {
	var body struct {
		Word string `json:"word"`
	}
	if !bindAdminJSON(c, &body, "invalid request body") {
		return
	}
	item, err := h.words.Create(c.Request.Context(), body.Word)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": item.ID, "word": item.Word, "hits": item.Hits, "created_at": item.CreatedAt}})
}

// Import bulk-adds words from a free-form text blob — split on newlines,
// commas (英文/中文), 顿号 and semicolons — skipping blanks and existing entries.
func (h *BannedWordsHandler) Import(c *gin.Context) {
	var body struct {
		Text string `json:"text"`
	}
	if !bindAdminJSON(c, &body, "请提供要导入的违禁词") {
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "请提供要导入的违禁词"})
		return
	}
	words := strings.FieldsFunc(body.Text, func(r rune) bool {
		switch r {
		case '\n', '\r', ',', '，', '、', ';', '；':
			return true
		}
		return false
	})
	added, skipped, err := h.words.BulkCreate(c.Request.Context(), words)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": "导入失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"added": added, "skipped": skipped})
}

// Hits — 违禁词触发列表: who triggered which word and when, newest first,
// with server-side pagination + ?q= search (违禁词/用户名/提示词, 跨页).
func (h *BannedWordsHandler) Hits(c *gin.Context) {
	limit := parseInt(c.Query("limit"), 50)
	offset := parseInt(c.Query("offset"), 0)
	items, total, err := h.words.ListHits(c.Request.Context(), strings.TrimSpace(c.Query("q")), limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": "failed to load banned word hits"})
		return
	}
	out := make([]gin.H, 0, len(items))
	for _, hit := range items {
		out = append(out, gin.H{
			"id":         hit.ID,
			"word":       hit.Word,
			"user_id":    hit.UserID,
			"user_name":  hit.UserName,
			"prompt":     hit.Prompt,
			"created_at": hit.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": out, "total": total})
}

func (h *BannedWordsHandler) Delete(c *gin.Context) {
	n, err := h.words.Delete(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": "delete failed"})
		return
	}
	if n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"detail": "not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
