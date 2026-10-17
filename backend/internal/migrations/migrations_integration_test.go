package migrations

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func migrationTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("IMAGENEXA_TEST_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL test database")
	}
	parent, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("connect to isolated PostgreSQL database")
	}
	parentSQL, err := parent.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parentSQL.Close() })
	var current string
	if parent.Raw("SELECT current_database()").Scan(&current).Error != nil || !strings.HasPrefix(current, "imagenexa_optimization_test_") {
		t.Fatal("refusing non-test database")
	}
	name := fmt.Sprintf("imagenexa_optimization_test_migrations_%d", time.Now().UnixNano())
	if err := parent.Exec(`CREATE DATABASE "` + name + `"`).Error; err != nil {
		t.Fatal("create empty migration test database:", err)
	}
	t.Cleanup(func() {
		if err := parent.Exec(`DROP DATABASE "` + name + `" WITH (FORCE)`).Error; err != nil {
			t.Error("drop migration test database:", err)
		}
	})
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("parse test database configuration")
	}
	cfg.Database = name
	sqlDB := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = sqlDB.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("connect to empty migration test database")
	}
	return db
}

func TestFreshAndLegacyDatabaseStartup(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			db := migrationTestDB(t)
			if legacy {
				if err := db.Exec(`CREATE TABLE refresh_profiles (
					id VARCHAR(64) PRIMARY KEY, name VARCHAR(255) NOT NULL, pool VARCHAR(64) NOT NULL,
					kind VARCHAR(64) NOT NULL, cookie TEXT
				);
				INSERT INTO refresh_profiles (id, name, pool, kind, cookie)
				VALUES ('legacy', 'Legacy Adobe', 'adobe', 'adobe_cookie', 'test-cookie')`).Error; err != nil {
					t.Fatal(err)
				}
			}
			// Match production: migrations run before operational AutoMigrate.
			if err := Run(context.Background(), db); err != nil {
				t.Fatal("cold startup:", err)
			}
			if err := db.AutoMigrate(model.AutoMigrateModels()...); err != nil {
				t.Fatal("operational schema:", err)
			}
			if err := Run(context.Background(), db); err != nil {
				t.Fatal("second startup:", err)
			}
			definitions, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			var applied []AppliedMigration
			if err := db.Raw("SELECT version, name, checksum FROM schema_migrations ORDER BY version").Scan(&applied).Error; err != nil {
				t.Fatal(err)
			}
			if len(applied) != len(definitions) {
				t.Fatal("not every migration was applied")
			}
			for i, definition := range definitions {
				if applied[i].Version != definition.Version || applied[i].Name != definition.Name || applied[i].Checksum != definition.Checksum {
					t.Fatal("migration history changed")
				}
			}
			if legacy {
				var profile model.RefreshProfile
				if err := db.First(&profile, "id = ?", "legacy").Error; err != nil || profile.Cookie != "test-cookie" || profile.ARPSessionToken == "" {
					t.Fatal("legacy refresh profile was not preserved and upgraded")
				}
			}
		})
	}
}

func TestConcurrentFreshDatabaseMigrations(t *testing.T) {
	db := migrationTestDB(t)
	var workers sync.WaitGroup
	errors := make(chan error, 2)
	for range 2 {
		workers.Go(func() { errors <- Run(context.Background(), db) })
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal("concurrent startup:", err)
		}
	}
}
