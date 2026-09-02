package router

import (
	"os"
	"strings"

	"backend/internal/config"
	"backend/internal/http/handler"
	"backend/internal/http/middleware"
	"backend/internal/service"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

// Handlers is intentionally limited to the 2API data plane and singleton
// administrator control plane. Public site, customer, billing, CDK, invite,
// recharge, and playground handlers do not belong in this router.
type Handlers struct {
	Health         *handler.HealthHandler
	V1             *handler.V1Handler
	Auth           *handler.AuthHandler
	APICredentials *handler.UserToolsHandler
	Admin          *handler.AdminConsoleHandler
	BannedWords    *handler.BannedWordsHandler
}

func New(cfg *config.Config, auth *service.AuthService, credentials *service.APICredentialService, handlers Handlers) *gin.Engine {
	if cfg.AppEnv != "development" {
		gin.SetMode(gin.ReleaseMode)
	}

	engine := gin.New()
	engine.Use(middleware.RequestID(), middleware.Recovery(os.Stderr))

	engine.GET("/health/live", handlers.Health.Live)
	engine.GET("/health/ready", handlers.Health.Ready)

	v1 := engine.Group("/v1")
	v1.Use(cors.New(cors.Config{
		AllowOriginFunc:  func(origin string) bool { return strings.TrimSpace(origin) != "" },
		AllowMethods:     []string{"GET", "POST", "OPTIONS"},
		AllowHeaders:     []string{"Authorization", "Content-Type", "Idempotency-Key", "Prefer", "X-Request-Id"},
		ExposeHeaders:    []string{"Idempotency-Key", "Location", "Preference-Applied", "Retry-After", "X-Request-Id"},
		AllowCredentials: false,
	}), middleware.RequireAPICredential(credentials))
	{
		v1.OPTIONS("/*path", preflight)
		v1.GET("/models", handlers.V1.Models)
		v1.POST("/chat/completions", handlers.V1.ChatCompletions)
		v1.POST("/images/generations", handlers.V1.ImageGenerations)
		v1.POST("/images/edits", handlers.V1.ImageEdits)
		v1.GET("/images/tasks", handlers.V1.GetImageTask)
		v1.GET("/images/:id/content", handlers.V1.GetImageContent)
		v1.POST("/videos", handlers.V1.CreateVideo)
		v1.GET("/videos/:id", handlers.V1.GetVideo)
		v1.GET("/videos/:id/content", handlers.V1.GetVideoContent)
	}

	admin := engine.Group("/admin/api")
	admin.Use(cors.New(cors.Config{
		AllowOrigins:     cfg.CORSOrigins,
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Accept", "Content-Type", middleware.CSRFHeaderName, handler.AdminBootstrapTokenHeader, "X-Request-Id"},
		ExposeHeaders:    []string{"X-Request-Id"},
		AllowCredentials: true,
	}))

	authPublic := admin.Group("/auth")
	admin.OPTIONS("/*path", preflight)
	{
		authPublic.GET("/status", handlers.Auth.Config)
		authPublic.POST("/initialize", middleware.RequireTrustedOrigin(cfg), handlers.Auth.Initialize)
		authPublic.POST("/login", middleware.RequireTrustedOrigin(cfg), handlers.Auth.Login)
	}

	authed := admin.Group("")
	authed.Use(middleware.RequireAdminSession(auth, cfg))
	{
		authed.GET("/auth/me", handlers.Auth.Me)
		authed.GET("/auth/csrf", handlers.Auth.Me)
		authed.POST("/auth/logout", handlers.Auth.Logout)
		authed.POST("/auth/change-password", handlers.Auth.ChangePassword)

		authed.GET("/api-credentials", handlers.APICredentials.APICredentialsList)
		authed.POST("/api-credentials", handlers.APICredentials.APICredentialsCreate)
		authed.PATCH("/api-credentials/:id", handlers.APICredentials.APICredentialsUpdate)
		authed.POST("/api-credentials/:id/rotate", handlers.APICredentials.APICredentialsRotate)
		authed.DELETE("/api-credentials/:id", handlers.APICredentials.APICredentialsRevoke)

		authed.GET("/logical-models", handlers.Admin.LogicalModels)
		authed.PATCH("/logical-models/:id", handlers.Admin.UpdateLogicalModel)
		authed.PATCH("/logical-models/:id/routes/:route_id", handlers.Admin.UpdateLogicalRoute)
		authed.GET("/overview", handlers.Admin.Overview)

		authed.GET("/accounts", handlers.Admin.Accounts)
		authed.POST("/accounts/import", handlers.Admin.ImportAccount)
		authed.POST("/accounts/delete-dead", handlers.Admin.DeleteDeadAccounts)
		authed.PATCH("/accounts/:id", handlers.Admin.UpdateAccount)
		authed.DELETE("/accounts/:id", handlers.Admin.DeleteAccount)
		authed.PATCH("/accounts/:id/routes/:binding_id", handlers.Admin.SetAccountRoute)
		authed.POST("/accounts/:id/refresh-quota", handlers.Admin.RefreshAccountQuota)
		authed.POST("/test", handlers.V1.AdminTest)
		authed.GET("/test/artifacts/:id", handlers.V1.AdminTestArtifact)

		authed.GET("/logs", handlers.Admin.Logs)
		authed.GET("/artifacts", handlers.Admin.Artifacts)
		authed.GET("/artifacts/:id/content", handlers.V1.AdminArtifact)
		authed.GET("/settings", handlers.Admin.Settings)
		authed.PUT("/settings", handlers.Admin.UpdateSettings)

		authed.GET("/banned-words", handlers.BannedWords.List)
		authed.POST("/banned-words", handlers.BannedWords.Create)
		authed.POST("/banned-words/import", handlers.BannedWords.Import)
		authed.DELETE("/banned-words/:id", handlers.BannedWords.Delete)
		authed.GET("/banned-word-hits", handlers.BannedWords.Hits)
	}

	return engine
}

func preflight(c *gin.Context) {
	c.Status(204)
}
