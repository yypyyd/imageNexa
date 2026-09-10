package bootstrap

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"backend/internal/config"
	"backend/internal/http/handler"
	"backend/internal/http/router"
	"backend/internal/migrations"
	"backend/internal/model"
	"backend/internal/provider/adobe"
	"backend/internal/provider/byteplus"
	"backend/internal/provider/chatgpt"
	"backend/internal/provider/custom"
	"backend/internal/provider/dola"
	"backend/internal/provider/grok"
	"backend/internal/provider/oreate"
	"backend/internal/provider/runway"
	"backend/internal/repo"
	"backend/internal/service"
	"backend/internal/storage"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type App struct {
	Config *config.Config
	DB     *gorm.DB
	Redis  *redis.Client
	Engine *gin.Engine
	oreate *oreate.Client
	cancel context.CancelFunc
}

func NewApp(ctx context.Context) (*App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.GeneratedRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create generated root %s: %w", cfg.GeneratedRoot, err)
	}

	dbLogger := logger.Default.LogMode(logger.Warn)
	if cfg.AppEnv == "production" {
		// Provider cookies, API keys and login material are query parameters. Never
		// interpolate them into production SQL logs, including slow-query output.
		dbLogger = logger.New(log.New(os.Stderr, "", log.LstdFlags), logger.Config{
			SlowThreshold: time.Second, LogLevel: logger.Warn, ParameterizedQueries: true,
		})
	}
	db, err := gorm.Open(postgres.Open(cfg.PostgresDSN), &gorm.Config{TranslateError: true, Logger: dbLogger})
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("sql db: %w", err)
	}
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetMaxOpenConns(20)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)

	// Forward-only migrations own identity, routing, provider accounts, quota,
	// dispatch, and idempotency constraints. AutoMigrate runs afterwards only to
	// add compatible columns on retained operational tables.
	if err := migrations.Run(ctx, db); err != nil {
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	if err := db.WithContext(ctx).AutoMigrate(model.AutoMigrateModels()...); err != nil {
		return nil, fmt.Errorf("auto migrate operational tables: %w", err)
	}
	if err := seedDefaults(ctx, db); err != nil {
		return nil, fmt.Errorf("seed defaults: %w", err)
	}

	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPassword, DB: cfg.RedisDB})
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}

	adminRepo := repo.NewAdminRepository(db)
	credentialRepo := repo.NewAPICredentialRepository(db)
	modelRepo := repo.NewModelRepository(db)
	eventRepo := repo.NewEventRepository(db)
	tokenRepo := repo.NewTokenRepository(db)
	refreshRepo := repo.NewRefreshProfileRepository(db)
	settingsRepo := repo.NewSiteSettingRepository(db, rdb)
	bannedRepo := repo.NewBannedWordRepository(db)

	sessionSvc := service.NewSessionService(rdb, cfg.SessionTTL, cfg.SessionSlideAfter)
	authSvc := service.NewAdminAuthService(adminRepo, sessionSvc, rdb)
	concurrencySvc := service.NewConcurrencyService(rdb)
	credentialSvc := service.NewAPICredentialService(credentialRepo)
	credentialSvc.SetConcurrency(concurrencySvc)
	rateLimitSvc := service.NewRateLimitService(rdb)

	adobeClient := adobe.NewClient("clio-playground-web", "")
	bytePlusClient := byteplus.NewClient("")
	chatGPTClient := chatgpt.NewClient("")
	runwayClient := runway.NewClient("")
	grokClient := grok.NewClient("")
	oreateClient := oreate.NewClient("")
	dolaClient := dola.NewClient("")
	customClient := custom.NewClient()
	objectStore := storage.New(cfg.RustFSEndpoint, cfg.RustFSBucket, cfg.RustFSAccessKey, cfg.RustFSSecretKey)

	tokenSvc := service.NewTokenService(
		tokenRepo, refreshRepo, eventRepo, settingsRepo,
		adobeClient, bytePlusClient, chatGPTClient, runwayClient,
		grokClient, oreateClient, dolaClient, customClient, modelRepo.Quotas(),
	)
	refreshSvc := service.NewRefreshProfileService(refreshRepo, tokenRepo, adobeClient, bytePlusClient, tokenSvc)
	v1Svc := service.NewV1Service(
		cfg, modelRepo, credentialSvc, eventRepo, tokenRepo, settingsRepo, concurrencySvc,
		adobeClient, bytePlusClient, chatGPTClient, runwayClient,
		grokClient, oreateClient, dolaClient, customClient, objectStore,
	)
	v1Svc.SetRefresh(refreshSvc)
	v1Svc.SetBannedWords(bannedRepo)
	adminConsoleSvc := service.NewAdminConsoleService(cfg, db, modelRepo, tokenRepo, tokenSvc, settingsRepo)
	maintenanceCtx, cancelMaintenance := context.WithCancel(ctx)
	maintenanceSvc := service.NewMaintenanceService(tokenRepo, tokenSvc, eventRepo, refreshSvc, settingsRepo, modelRepo, objectStore, v1Svc)
	go maintenanceSvc.Run(maintenanceCtx)

	engine := router.New(cfg, authSvc, credentialSvc, router.Handlers{
		Health:         handler.NewHealthHandler(db, rdb, objectStore),
		V1:             handler.NewV1Handler(v1Svc),
		Auth:           handler.NewAuthHandler(cfg, authSvc, rateLimitSvc),
		APICredentials: handler.NewUserToolsHandler(credentialSvc),
		Admin:          handler.NewAdminConsoleHandler(adminConsoleSvc),
		BannedWords:    handler.NewBannedWordsHandler(bannedRepo),
	})

	return &App{Config: cfg, DB: db, Redis: rdb, Engine: engine, oreate: oreateClient, cancel: cancelMaintenance}, nil
}

func (a *App) Close() error {
	var firstErr error
	if a.cancel != nil {
		a.cancel()
	}
	if a.oreate != nil {
		a.oreate.Close()
	}
	if a.Redis != nil {
		if err := a.Redis.Close(); err != nil {
			firstErr = err
		}
	}
	if a.DB != nil {
		sqlDB, err := a.DB.DB()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else if err := sqlDB.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
