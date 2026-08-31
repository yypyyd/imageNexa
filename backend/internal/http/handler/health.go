package handler

import (
	"context"
	"net/http"
	"time"

	"backend/internal/migrations"
	"backend/internal/storage"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// HealthHandler deliberately keeps liveness independent from external
// services. A temporary database or Redis outage must make the instance
// unready without causing an orchestrator to restart an otherwise healthy
// process in a tight loop.
type HealthHandler struct {
	db    *gorm.DB
	redis *redis.Client
	store *storage.Client
}

func NewHealthHandler(db *gorm.DB, redisClient *redis.Client, objectStore ...*storage.Client) *HealthHandler {
	var store *storage.Client
	if len(objectStore) > 0 {
		store = objectStore[0]
	}
	return &HealthHandler{db: db, redis: redisClient, store: store}
}

func (h *HealthHandler) Live(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *HealthHandler) Ready(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	if h.db == nil || h.redis == nil || h.store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready"})
		return
	}

	sqlDB, err := h.db.DB()
	if err != nil || sqlDB.PingContext(ctx) != nil || h.redis.Ping(ctx).Err() != nil || h.store.Check(ctx) != nil || !schemaIsCurrent(ctx, h.db) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ready"})
}

func schemaIsCurrent(ctx context.Context, db *gorm.DB) bool {
	loaded, err := migrations.Load()
	if err != nil || len(loaded) == 0 {
		return false
	}
	version, err := migrations.CurrentVersion(ctx, db)
	return err == nil && version == loaded[len(loaded)-1].Version
}
