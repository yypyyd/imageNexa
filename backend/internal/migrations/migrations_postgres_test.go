package migrations

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// TestPostgresLegacyUpgrade is intentionally opt-in. It creates and drops a
// uniquely named database on the supplied PostgreSQL server; the DSN itself is
// never modified. CI and local audits can enable it with:
//
//	MIGRATIONS_TEST_POSTGRES_DSN="host=postgres user=postgres password=... dbname=audit sslmode=disable" go test ./internal/migrations
func TestPostgresLegacyUpgrade(t *testing.T) {
	dsn := migrationTestDSN(t)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	db := newMigrationTestDatabase(t, ctx, dsn)

	execFixtureSQL(t, db, legacySchemaAndDataSQL)
	migrations, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(migrations) != 9 {
		t.Fatalf("loaded %d migrations, want 9", len(migrations))
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("resolve SQL DB: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `
		CREATE TABLE schema_migrations (
			version BIGINT PRIMARY KEY,
			name TEXT NOT NULL,
			checksum CHAR(64) NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire migration connection: %v", err)
	}
	defer conn.Close()
	for _, migration := range migrations[:3] {
		if err := applyOne(ctx, conn, migration); err != nil {
			t.Fatalf("apply migration %06d: %v", migration.Version, err)
		}
	}

	execFixtureSQL(t, db, routingAndQuotaFixtureSQL)
	if err := applyOne(ctx, conn, migrations[3]); err != nil {
		t.Fatalf("apply migration 000004: %v", err)
	}
	if err := applyOne(ctx, conn, migrations[4]); err != nil {
		t.Fatalf("apply migration 000005: %v", err)
	}
	if err := applyOne(ctx, conn, migrations[5]); err != nil {
		t.Fatalf("apply migration 000006: %v", err)
	}
	if err := applyOne(ctx, conn, migrations[6]); err != nil {
		t.Fatalf("apply migration 000007: %v", err)
	}
	if err := applyOne(ctx, conn, migrations[7]); err != nil {
		t.Fatalf("apply migration 000008: %v", err)
	}
	if err := applyOne(ctx, conn, migrations[8]); err != nil {
		t.Fatalf("apply migration 000009: %v", err)
	}

	assertLegacyIdentity(t, db)
	assertClosedCatalogAndCustomRoutes(t, db)
	assertAdobeQuotaMerge(t, db)
	assertRetiredDataScrubbed(t, db)
	assertEventIdentityColumns(t, db)
	assertLegacyIdentityScrubbed(t, db)
	assertLegacyTablesRemoved(t, db)
	assertAdobeARPSessionsBackfilled(t, db)
	assertQuotaReservationsCascadeOnAccountDelete(t, db)

	// A normal second startup must be a checksum-validated no-op.
	if err := conn.Close(); err != nil {
		t.Fatalf("close fixture connection: %v", err)
	}
	if err := Run(ctx, db); err != nil {
		t.Fatalf("second migration Run() error = %v", err)
	}
	assertInt64(t, db, "SELECT COUNT(*) FROM schema_migrations", 9)
	assertInt64(t, db, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations", 9)
}

func TestPostgresDisabledAdminRemainsBootstrapable(t *testing.T) {
	dsn := migrationTestDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	db := newMigrationTestDatabase(t, ctx, dsn)
	execFixtureSQL(t, db, disabledAdminOnlyFixtureSQL)

	if err := Run(ctx, db); err != nil {
		t.Fatalf("Run() disabled-admin fixture error = %v", err)
	}
	assertInt64(t, db, "SELECT COUNT(*) FROM admins", 0)
	assertInt64(t, db, "SELECT COUNT(*) FROM api_credentials", 0)
	assertLegacyTablesRemoved(t, db)

	// This is the repository-level write performed by bootstrap after its
	// high-entropy deployment token has been verified.
	execFixtureSQL(t, db, `
		INSERT INTO admins
			(id,singleton_key,username,email,password_hash,status,session_version)
		VALUES
			('admin',1,'newadmin','new@example.test',
			 'sha256$salt$aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
			 'active',1)
	`)
	assertInt64(t, db, "SELECT COUNT(*) FROM admins WHERE status = 'active'", 1)
}

func TestPostgresLegacyKeyStatusesArePreserved(t *testing.T) {
	dsn := migrationTestDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	db := newMigrationTestDatabase(t, ctx, dsn)
	execFixtureSQL(t, db, legacyKeyStatusFixtureSQL)

	if err := Run(ctx, db); err != nil {
		t.Fatalf("Run() legacy-key-status fixture error = %v", err)
	}
	assertInt64(t, db, "SELECT COUNT(*) FROM admins WHERE username = 'activeadmin' AND status = 'active'", 1)
	assertInt64(t, db, "SELECT COUNT(*) FROM api_credentials", 3)
	assertInt64(t, db, "SELECT COUNT(*) FROM api_credentials WHERE id = 'key-active' AND status = 'active' AND revoked_at IS NULL", 1)
	assertInt64(t, db, "SELECT COUNT(*) FROM api_credentials WHERE id = 'key-active-disabled' AND status = 'disabled' AND revoked_at IS NULL", 1)
	assertInt64(t, db, "SELECT COUNT(*) FROM api_credentials WHERE id = 'key-active-revoked' AND status = 'revoked' AND revoked_at IS NOT NULL", 1)
	assertLegacyTablesRemoved(t, db)
}

func migrationTestDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("MIGRATIONS_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set MIGRATIONS_TEST_POSTGRES_DSN to run the PostgreSQL migration fixture")
	}
	return dsn
}

func newMigrationTestDatabase(t *testing.T, ctx context.Context, dsn string) *gorm.DB {
	t.Helper()
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse MIGRATIONS_TEST_POSTGRES_DSN: %v", err)
	}
	adminDB := stdlib.OpenDB(*config)
	if err := adminDB.PingContext(ctx); err != nil {
		adminDB.Close()
		t.Fatalf("connect to migration test PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("twoapi_migration_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedName := `"` + strings.ReplaceAll(databaseName, `"`, `""`) + `"`
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+quotedName); err != nil {
		adminDB.Close()
		t.Fatalf("create isolated migration database: %v", err)
	}

	testConfig := config.Copy()
	testConfig.Database = databaseName
	testSQLDB := stdlib.OpenDB(*testConfig)
	t.Cleanup(func() {
		_ = testSQLDB.Close()
		dropCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := adminDB.ExecContext(dropCtx, "DROP DATABASE IF EXISTS "+quotedName+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated migration database: %v", err)
		}
		_ = adminDB.Close()
	})
	if err := testSQLDB.PingContext(ctx); err != nil {
		t.Fatalf("connect to isolated migration database: %v", err)
	}
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: testSQLDB}), &gorm.Config{})
	if err != nil {
		t.Fatalf("open isolated migration database with GORM: %v", err)
	}
	return db
}

func execFixtureSQL(t *testing.T, db *gorm.DB, statement string) {
	t.Helper()
	if err := db.Exec(statement).Error; err != nil {
		t.Fatalf("execute migration fixture: %v", err)
	}
}

func assertInt64(t *testing.T, db *gorm.DB, query string, want int64, args ...any) {
	t.Helper()
	var got int64
	if err := db.Raw(query, args...).Scan(&got).Error; err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	if got != want {
		t.Fatalf("query %q = %d, want %d", query, got, want)
	}
}

func assertLegacyIdentity(t *testing.T, db *gorm.DB) {
	t.Helper()
	var admin struct {
		Username string
		Status   string
	}
	if err := db.Raw("SELECT username, status FROM admins WHERE singleton_key = 1").Scan(&admin).Error; err != nil {
		t.Fatalf("read migrated admin: %v", err)
	}
	if admin.Username != "activeadmin" || admin.Status != "active" {
		t.Fatalf("migrated admin = %#v, want activeadmin/active", admin)
	}

	var credential struct {
		ID     string
		Status string
	}
	if err := db.Raw("SELECT id, status FROM api_credentials WHERE id = 'key-active'").Scan(&credential).Error; err != nil {
		t.Fatalf("read migrated credential: %v", err)
	}
	if credential.ID != "key-active" || credential.Status != "active" {
		t.Fatalf("migrated credential = %#v, want key-active/active", credential)
	}
	assertInt64(t, db, "SELECT COUNT(*) FROM api_credentials", 1)
}

func assertClosedCatalogAndCustomRoutes(t *testing.T, db *gorm.DB) {
	t.Helper()
	assertInt64(t, db, "SELECT COUNT(*) FROM logical_models WHERE enabled", 19)
	assertInt64(t, db, "SELECT COUNT(*) FROM model_routes WHERE provider IN ('chatgpt','byteplus','adobe','runway','grok','oreate')", 27)
	assertInt64(t, db, "SELECT COUNT(*) FROM model_routes WHERE provider = 'custom'", 19)
	assertInt64(t, db, "SELECT COUNT(*) FROM account_model_routes WHERE account_id = 'custom-specified'", 2)
	assertInt64(t, db, "SELECT COUNT(*) FROM account_model_routes WHERE account_id = 'custom-all'", 19)
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM account_model_routes
		WHERE account_id = 'custom-specified'
		  AND model_route_id IN ('custom.gpt-image-2', 'custom.seedance-2.0')
		  AND enabled AND entitled`, 2)
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM model_routes
		WHERE provider = 'custom'
		  AND quota_costs = '{"mode":"unmetered"}'::jsonb`, 19)
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM logical_models
		WHERE id IN ('luma-ray','runway-gen-4-turbo','runway-gen-4.5','veo-3.1','veo-3.1-lite')`, 0)
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM model_routes
		WHERE quota_costs IS NULL
		   OR quota_costs->>'mode' NOT IN ('metered','unknown','unmetered')`, 0)

	// A canonical route that an administrator disabled remains disabled, and its
	// binding is not confused with an invalid/retired route.
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM model_routes
		WHERE id = 'image.gpt-image-2.byteplus' AND NOT enabled`, 1)
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM account_model_routes
		WHERE account_id = 'byte-disabled'
		  AND model_route_id = 'image.gpt-image-2.byteplus'
		  AND NOT enabled AND NOT entitled`, 1)

	assertInt64(t, db, "SELECT COUNT(*) FROM model_routes WHERE id = 'legacy.leonardo.nohistory'", 0)
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM model_routes
		WHERE id = 'legacy.leonardo.history' AND NOT enabled`, 1)
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM account_model_routes
		WHERE model_route_id = 'legacy.leonardo.history'
		  AND NOT enabled AND NOT entitled`, 1)
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM pg_class
		WHERE relname LIKE 'migration_000004_%'`, 0)
}

func assertAdobeQuotaMerge(t *testing.T, db *gorm.DB) {
	t.Helper()
	var bucket struct {
		ID        string
		Remaining float64
		Reserved  float64
	}
	if err := db.Raw(`
		SELECT id, remaining, reserved
		FROM account_quota_buckets
		WHERE account_id = 'adobe-main' AND bucket_key = 'adobe.credits'`).Scan(&bucket).Error; err != nil {
		t.Fatalf("read merged Adobe bucket: %v", err)
	}
	if bucket.ID != "manual-adobe-shared" || bucket.Remaining != 78 || bucket.Reserved != 14 {
		t.Fatalf("merged Adobe bucket = %#v, want manual-adobe-shared remaining=78 reserved=14", bucket)
	}
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM account_quota_buckets
		WHERE account_id = 'adobe-main'
		  AND bucket_key IN ('adobe.image','adobe.video')`, 0)
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM quota_reservations
		WHERE id IN ('adobe-hold-image','adobe-hold-video','adobe-hold-shared')
		  AND quota_bucket_id = 'manual-adobe-shared'`, 3)
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM model_routes
		WHERE provider = 'adobe' AND quota_bucket_key <> 'adobe.credits'`, 0)
}

func assertAdobeARPSessionsBackfilled(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, query := range []string{
		`SELECT arp_session_token FROM provider_accounts WHERE id = 'adobe-main'`,
		`SELECT arp_session_token FROM refresh_profiles WHERE id = 'keep-adobe'`,
	} {
		var token string
		if err := db.Raw(query).Scan(&token).Error; err != nil {
			t.Fatalf("read backfilled Adobe ARP token: %v", err)
		}
		if !strings.HasPrefix(token, "eyJzaWQiOiI") {
			t.Fatalf("backfilled Adobe ARP token has unexpected shape: %q", token)
		}
	}
}

func assertQuotaReservationsCascadeOnAccountDelete(t *testing.T, db *gorm.DB) {
	t.Helper()
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM quota_reservations reservation
		JOIN account_quota_buckets bucket ON bucket.id = reservation.quota_bucket_id
		WHERE bucket.account_id = 'adobe-main'`, 3)
	if err := db.Exec("DELETE FROM provider_accounts WHERE id = ?", "adobe-main").Error; err != nil {
		t.Fatalf("delete account with quota reservation history: %v", err)
	}
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM quota_reservations
		WHERE id IN ('adobe-hold-image','adobe-hold-video','adobe-hold-shared')`, 0)
}

func assertRetiredDataScrubbed(t *testing.T, db *gorm.DB) {
	t.Helper()
	assertInt64(t, db, "SELECT COUNT(*) FROM provider_accounts WHERE id = 'leo-nohistory'", 0)
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM provider_accounts
		WHERE id = 'leo-history'
		  AND status = 'disabled' AND dead
		  AND value = '' AND meta = '{}'::jsonb
		  AND account_email = '' AND account_display_name = ''`, 1)
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM provider_accounts
		WHERE id = 'chat-allowed'
		  AND value = 'chat-secret'
		  AND account_email = 'chat@example.test'
		  AND account_display_name = 'Chat'`, 1)
	assertInt64(t, db, "SELECT COUNT(*) FROM refresh_profiles", 1)
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM refresh_profiles
		WHERE pool = 'adobe' AND kind = 'adobe_cookie'`, 1)
}

func assertEventIdentityColumns(t *testing.T, db *gorm.DB) {
	t.Helper()
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'event_logs'
		  AND column_name IN ('api_credential_id','request_fingerprint','response_format','mime_type')
		  AND is_nullable = 'NO'`, 3)
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM pg_constraint
		WHERE conrelid = 'event_logs'::regclass
		  AND conname = 'event_logs_response_format_valid'`, 1)
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM event_logs
		WHERE id = 'event-active' AND api_credential_id = 'key-active'`, 1)
}

func assertLegacyIdentityScrubbed(t *testing.T, db *gorm.DB) {
	t.Helper()
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'event_logs'
		  AND column_name IN ('user_id','user_name')`, 0)
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM banned_word_hits
		WHERE id = 'legacy-hit' AND user_id = '' AND user_name = ''`, 1)
}

func assertLegacyTablesRemoved(t *testing.T, db *gorm.DB) {
	t.Helper()
	assertInt64(t, db, `
		SELECT COUNT(*)
		FROM unnest(ARRAY[
			'public.users',
			'public.api_keys',
			'public.model_configs',
			'public.token_accounts'
		]) AS legacy_table(name)
		WHERE to_regclass(legacy_table.name) IS NOT NULL`, 0)
}

const legacySchemaAndDataSQL = `
CREATE TABLE users (
    id VARCHAR(32) PRIMARY KEY,
    name VARCHAR(64),
    email VARCHAR(255),
    password_hash VARCHAR(255),
    status VARCHAR(32),
    role VARCHAR(32),
    last_login_at TIMESTAMPTZ,
    last_login_ip VARCHAR(128),
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ
);

CREATE TABLE api_keys (
    id VARCHAR(32) PRIMARY KEY,
    user_id VARCHAR(32) NOT NULL REFERENCES users(id),
    name VARCHAR(100),
    key_preview VARCHAR(32),
    key_hash VARCHAR(255),
	status VARCHAR(32) NOT NULL DEFAULT 'active',
	revoked_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ
);

CREATE TABLE token_accounts (
    id VARCHAR(64) PRIMARY KEY,
    pool VARCHAR(64) NOT NULL,
    value TEXT,
    status VARCHAR(32) NOT NULL DEFAULT 'active',
    fails INTEGER NOT NULL DEFAULT 0,
    fail_total INTEGER NOT NULL DEFAULT 0,
    upstream_fails INTEGER NOT NULL DEFAULT 0,
    success_total INTEGER NOT NULL DEFAULT 0,
    dead BOOLEAN NOT NULL DEFAULT FALSE,
    meta JSONB,
    added_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    cached_quota_reset_after VARCHAR(128),
    quota_recover_at TIMESTAMPTZ,
    image_limited BOOLEAN NOT NULL DEFAULT FALSE,
    video_limited BOOLEAN NOT NULL DEFAULT FALSE,
    account_email VARCHAR(255),
    account_display_name VARCHAR(255),
    weight INTEGER NOT NULL DEFAULT 0,
    concurrency INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ
);

CREATE TABLE refresh_profiles (
    id VARCHAR(64) PRIMARY KEY,
    pool VARCHAR(64) NOT NULL,
    kind VARCHAR(64) NOT NULL,
    value TEXT NOT NULL DEFAULT ''
);

CREATE TABLE model_configs (
    id VARCHAR(255) PRIMARY KEY,
    provider VARCHAR(100) NOT NULL,
    name VARCHAR(255) NOT NULL
);

CREATE TABLE event_logs (
    id VARCHAR(32) PRIMARY KEY,
    user_id VARCHAR(32) NOT NULL DEFAULT '',
    status VARCHAR(32) NOT NULL,
    model VARCHAR(255) NOT NULL DEFAULT '',
    source VARCHAR(32) NOT NULL DEFAULT '',
    kind VARCHAR(32) NOT NULL DEFAULT '',
    request_id VARCHAR(191) NOT NULL DEFAULT ''
);

CREATE TABLE banned_word_hits (
    id VARCHAR(32) PRIMARY KEY,
    user_id VARCHAR(32) NOT NULL DEFAULT '',
    user_name VARCHAR(255) NOT NULL DEFAULT ''
);

INSERT INTO users
    (id,name,email,password_hash,status,role,last_login_ip,created_at,updated_at)
VALUES
    ('disabled-user','disabledadmin','disabled@example.test','sha256$salt$bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb','disabled','admin','',TIMESTAMPTZ '2024-01-01 00:00:00+00',NOW()),
    ('active-user','activeadmin','active@example.test','sha256$salt$aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','active','admin','',TIMESTAMPTZ '2025-01-01 00:00:00+00',NOW()),
    ('invalid-hash','invalidhash','invalid@example.test','not-a-real-hash','active','admin','',TIMESTAMPTZ '2022-01-01 00:00:00+00',NOW()),
    ('empty-password','emptypassword','empty@example.test','','active','admin','',TIMESTAMPTZ '2023-01-01 00:00:00+00',NOW());

INSERT INTO api_keys (id,user_id,name,key_preview,key_hash,status,revoked_at,created_at) VALUES
    ('key-disabled','disabled-user','disabled key','sk-old','hash-key-disabled','active',NULL,NOW()),
    ('key-active','active-user','active key',NULL,'hash-key-active','active',NULL,NULL),
    ('key-invalid','invalid-hash','invalid hash key','sk-invalid','hash-key-invalid','active',NULL,NOW()),
    ('key-empty','empty-password','empty password key','sk-empty','hash-key-empty','active',NULL,NOW());

INSERT INTO token_accounts (id,pool,value,meta,account_email,account_display_name) VALUES
    ('chat-allowed','chatgpt','chat-secret','{"cached_quota_total":"10","cached_quota_remaining":"9"}','chat@example.test','Chat'),
    ('byte-disabled','byteplus',NULL,'{}','byte@example.test','Byte'),
    ('adobe-main','adobe','adobe-secret','{}','adobe@example.test','Adobe'),
    ('custom-specified','custom','https://custom.example/v1|custom-secret','{"models":"gpt-image-2, seedance-2.0, unknown-retired"}','custom1@example.test','Custom One'),
    ('custom-all','custom','https://all.example/v1|all-secret','{}','custom2@example.test','Custom All'),
    ('legacy-token-only','leonardo','legacy-plaintext','{"secret":"legacy-meta"}','legacy@example.test','Legacy');

INSERT INTO refresh_profiles (id,pool,kind,value) VALUES
    ('keep-adobe','adobe','adobe_cookie','cookie-secret'),
    ('drop-adobe-kind','adobe','access_token','access-secret'),
    ('drop-retired','leonardo','cookie','retired-secret');

INSERT INTO model_configs (id,provider,name) VALUES
    ('seedream-legacy-obscure','leonardo','Retired Legacy Model');

INSERT INTO event_logs (id,user_id,status,model,source,kind,request_id) VALUES
    ('event-active','active-user','success','lumina-gpt-image-2','v1','image','request-active'),
    ('event-disabled','disabled-user','success','firefly-ray','v1','video','request-disabled');

INSERT INTO banned_word_hits (id,user_id,user_name) VALUES
    ('legacy-hit','ordinary-user','ordinary@example.test');
`

const disabledAdminOnlyFixtureSQL = `
CREATE TABLE users (
    id VARCHAR(32) PRIMARY KEY,
    name VARCHAR(64),
    email VARCHAR(255),
    password_hash VARCHAR(255),
    status VARCHAR(32),
    role VARCHAR(32),
    last_login_at TIMESTAMPTZ,
    last_login_ip VARCHAR(128),
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ
);

CREATE TABLE api_keys (
    id VARCHAR(32) PRIMARY KEY,
    user_id VARCHAR(32) NOT NULL REFERENCES users(id),
    name VARCHAR(100),
    key_preview VARCHAR(32),
    key_hash VARCHAR(255),
    created_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ
);

INSERT INTO users
    (id,name,email,password_hash,status,role,last_login_ip,created_at,updated_at)
VALUES
    ('disabled-only','disabledonly','disabled-only@example.test',
     'sha256$salt$cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc',
     'disabled','admin','',NOW(),NOW());

INSERT INTO api_keys
    (id,user_id,name,key_preview,key_hash,created_at)
VALUES
    ('disabled-only-key','disabled-only','disabled only key','sk-disabled','disabled-only-key-hash',NOW());
`

const legacyKeyStatusFixtureSQL = `
CREATE TABLE users (
    id VARCHAR(32) PRIMARY KEY,
    name VARCHAR(64),
    email VARCHAR(255),
    password_hash VARCHAR(255),
    status VARCHAR(32),
    role VARCHAR(32),
    last_login_at TIMESTAMPTZ,
    last_login_ip VARCHAR(128),
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ
);

CREATE TABLE api_keys (
    id VARCHAR(32) PRIMARY KEY,
    user_id VARCHAR(32) NOT NULL REFERENCES users(id),
    name VARCHAR(100),
    key_preview VARCHAR(32),
    key_hash VARCHAR(255),
    status VARCHAR(32) NOT NULL DEFAULT 'active',
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ
);

INSERT INTO users
    (id,name,email,password_hash,status,role,last_login_ip,created_at,updated_at)
VALUES
    ('active-user','activeadmin','active@example.test',
     'sha256$salt$aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
     'active','admin','',NOW(),NOW());

INSERT INTO api_keys
    (id,user_id,name,key_preview,key_hash,status,revoked_at,created_at)
VALUES
    ('key-active','active-user','active key','sk-active','hash-key-active','active',NULL,NOW()),
    ('key-active-disabled','active-user','disabled key','sk-disabled','hash-key-disabled','disabled',NULL,NOW()),
    ('key-active-revoked','active-user','revoked key','sk-revoked','hash-key-revoked','revoked',TIMESTAMPTZ '2026-01-01 00:00:00+00',NOW());
`

const routingAndQuotaFixtureSQL = `
UPDATE model_routes
SET enabled = FALSE
WHERE id = 'image.gpt-image-2.byteplus';

UPDATE account_model_routes
SET enabled = FALSE, entitled = FALSE
WHERE account_id = 'byte-disabled'
  AND model_route_id = 'image.gpt-image-2.byteplus';

INSERT INTO provider_accounts
    (id,pool,value,status,dead,meta,account_email,account_display_name)
VALUES
    ('leo-nohistory','leonardo','delete-secret','active',FALSE,'{"delete":"me"}','delete@example.test','Delete Me'),
    ('leo-history','leonardo','history-secret','active',FALSE,'{"keep":"history"}','history@example.test','History');

INSERT INTO model_routes
    (id,logical_model_id,provider,runtime_model,upstream_model,enabled,priority,weight,quota_bucket_key,quota_costs,capabilities)
VALUES
    ('legacy.leonardo.nohistory','gpt-image-2','leonardo','legacy-nohistory','legacy-nohistory',TRUE,999,1,'leonardo.credits','{}','[]'),
    ('legacy.leonardo.history','gpt-image-2','leonardo','legacy-history','legacy-history',TRUE,998,1,'leonardo.credits','{}','[]');

INSERT INTO account_model_routes
    (id,account_id,model_route_id,enabled,entitled,quota_bucket_key)
VALUES
    ('leo-nohistory:route','leo-nohistory','legacy.leonardo.nohistory',TRUE,TRUE,'leonardo.credits'),
    ('leo-history:route','leo-history','legacy.leonardo.history',TRUE,TRUE,'leonardo.credits');

INSERT INTO dispatch_attempts
    (id,event_id,model_route_id,account_id,state)
VALUES
    ('dispatch-leo-history','event-leo-history','legacy.leonardo.history','leo-history','succeeded');

INSERT INTO account_quota_buckets
    (id,account_id,bucket_key,unit,total,remaining,reserved,revision,refreshed_at)
VALUES
    ('leo-history:credits','leo-history','leonardo.credits','credits',10,9,0,1,NOW());

INSERT INTO quota_reservations
    (id,event_id,quota_bucket_id,amount,status,settled_at)
VALUES
    ('leo-history-settled','event-leo-history','leo-history:credits',1,'settled',NOW());

UPDATE account_quota_buckets
SET total = 100, remaining = 70, reserved = 5, revision = 10,
    refreshed_at = TIMESTAMPTZ '2026-01-01 00:00:00+00'
WHERE account_id = 'adobe-main' AND bucket_key = 'adobe.image';

UPDATE account_quota_buckets
SET total = 100, remaining = 50, reserved = 7, revision = 11,
    refreshed_at = TIMESTAMPTZ '2026-01-02 00:00:00+00'
WHERE account_id = 'adobe-main' AND bucket_key = 'adobe.video';

INSERT INTO account_quota_buckets
    (id,account_id,bucket_key,unit,total,remaining,reserved,revision,refreshed_at)
VALUES
    ('manual-adobe-shared','adobe-main','adobe.credits','credits',100,90,2,12,TIMESTAMPTZ '2026-01-03 00:00:00+00');

INSERT INTO quota_reservations (id,event_id,quota_bucket_id,amount,status) VALUES
    ('adobe-hold-image','event-adobe-image','adobe-main:adobe.image',5,'held'),
    ('adobe-hold-video','event-adobe-video','adobe-main:adobe.video',7,'uncertain'),
    ('adobe-hold-shared','event-adobe-shared','manual-adobe-shared',2,'held');
`
