package migrations

import (
	"regexp"
	"strings"
	"testing"

	"backend/internal/model"
)

func loadedMigrationSQL(t *testing.T, version int64) string {
	t.Helper()
	migrations, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	for _, migration := range migrations {
		if migration.Version == version {
			return migration.SQL
		}
	}
	t.Fatalf("migration %06d not loaded", version)
	return ""
}

func TestLoadMigrationsIsOrderedAndChecksummed(t *testing.T) {
	migrations, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("Load() returned no migrations")
	}
	if len(migrations) != 5 {
		t.Fatalf("Load() returned %d migrations, want 5", len(migrations))
	}
	hexChecksum := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for index, migration := range migrations {
		if migration.Version != int64(index+1) {
			t.Fatalf("migration %s has version %d, want %d", migration.Filename, migration.Version, index+1)
		}
		if index > 0 && migrations[index-1].Version >= migration.Version {
			t.Fatalf("migrations not strictly ordered: %d then %d", migrations[index-1].Version, migration.Version)
		}
		if !hexChecksum.MatchString(migration.Checksum) {
			t.Fatalf("migration %s has invalid checksum %q", migration.Filename, migration.Checksum)
		}
		if strings.Contains(migration.SQL, "\r") {
			t.Fatalf("migration %s is not LF-only", migration.Filename)
		}
	}
}

func TestRetiredVideoModelMigrationPreservesHistoryTombstones(t *testing.T) {
	sql := loadedMigrationSQL(t, 5)
	for _, required := range []string{
		"luma-ray", "runway-gen-4-turbo", "runway-gen-4.5", "veo-3.1", "veo-3.1-lite",
		"UPDATE logical_models", "UPDATE model_routes", "DELETE FROM account_model_routes",
		"DELETE FROM model_routes AS route", "DELETE FROM logical_models AS logical",
		"NOT EXISTS", "FROM dispatch_attempts AS attempt",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("retired-video migration missing %q", required)
		}
	}
}

func TestIdentityMigrationCarriesLegacyAdminKeyHashes(t *testing.T) {
	sql := loadedMigrationSQL(t, 1)
	for _, required := range []string{
		"admins_singleton_unique",
		"api_credentials_key_hash_unique",
		"concurrency_limit >= 0",
		"FROM api_keys",
		"k.key_hash",
		"WHERE role = 'admin'",
		"u.status = 'active'",
		`^bcrypt\$\$2[aby]`,
		`^sha256\$[^$]+\$[0-9a-fA-F]{64}`,
		"to_jsonb(k)->>'status'",
		"WHEN 'disabled' THEN 'disabled'",
		"WHEN 'revoked' THEN 'revoked'",
		"NULLIF(to_jsonb(k)->>'revoked_at', '')::timestamptz",
		"COALESCE(k.key_preview, '')",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("identity migration missing %q", required)
		}
	}
}

func TestEventIdentityMigrationPersistsResponseShape(t *testing.T) {
	sql := loadedMigrationSQL(t, 3)
	for _, required := range []string{
		"response_format VARCHAR(16) NOT NULL DEFAULT ''",
		"mime_type VARCHAR(100) NOT NULL DEFAULT ''",
		"ADD COLUMN IF NOT EXISTS response_format",
		"ADD COLUMN IF NOT EXISTS mime_type",
		"event_logs_response_format_valid",
		"CHECK (response_format IN ('', 'url', 'b64_json'))",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("event identity migration missing %q", required)
		}
	}
}

func TestRoutingMigrationPreservesNullableLegacyAccounts(t *testing.T) {
	sql := loadedMigrationSQL(t, 2)
	for _, required := range []string{
		"CONSTRAINT ux_route_identity UNIQUE",
		"CONSTRAINT ux_account_route UNIQUE",
		"CONSTRAINT ux_account_bucket UNIQUE",
		"COALESCE(value,'')",
		"COALESCE(created_at,NOW())",
		"COALESCE(updated_at,created_at,NOW())",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("routing migration missing %q", required)
		}
	}
}

func TestQuotaCostMigrationClosesCatalogAndMergesAdobeAllowance(t *testing.T) {
	sql := loadedMigrationSQL(t, 4)
	for _, required := range []string{
		`"calculator":"byteplus_image"`,
		`"calculator":"oreate_seedance"`,
		`"calculator":"per_second"`,
		`"mode":"unknown"`,
		`"mode":"unmetered"`,
		"adobe.credits",
		"UPDATE quota_reservations",
		"target.id",
		"migration_000004_canonical_routes",
		"migration_000004_invalid_routes",
		"migration_000004_custom_bindings",
		"regexp_split_to_table",
		"provider = 'custom'",
		"pool <> 'adobe' OR kind <> 'adobe_cookie'",
		"DROP TABLE IF EXISTS api_keys",
		"DROP TABLE IF EXISTS users",
		"DROP TABLE IF EXISTS model_configs",
		"DROP TABLE IF EXISTS token_accounts",
		"DROP COLUMN IF EXISTS user_id",
		"DROP COLUMN IF EXISTS user_name",
		"UPDATE banned_word_hits SET user_id = ''",
		"UPDATE banned_word_hits SET user_name = ''",
		"model_routes_quota_costs_valid",
		"DELETE FROM model_routes",
		"DELETE FROM provider_accounts",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("quota-cost migration missing %q", required)
		}
	}

	canonicalRoute := regexp.MustCompile(`(?m)^ \('([^']+)','([^']+)','([^']+)','([^']+)','([^']*)'\)[,;]$`)
	matches := canonicalRoute.FindAllStringSubmatch(sql, -1)
	if got := len(matches); got != 32 {
		t.Fatalf("quota-cost migration contains %d canonical route tuples, want 32", got)
	}
	want := make(map[string]struct{})
	for _, definition := range model.CanonicalRoutingCatalog() {
		for _, route := range definition.Routes {
			want[strings.Join([]string{route.ID, route.LogicalModelID, route.Provider, route.RuntimeModel, route.UpstreamModel}, "\x00")] = struct{}{}
		}
	}
	if len(want) != 27 {
		t.Fatalf("Go catalog contains %d routes, want 27", len(want))
	}
	retired := map[string]struct{}{
		strings.Join([]string{"video.veo-3.1.adobe", "veo-3.1", "adobe", "gemini-veo31", ""}, "\x00"):                                        {},
		strings.Join([]string{"video.veo-3.1-lite.adobe", "veo-3.1-lite", "adobe", "gemini-veo31-lite", ""}, "\x00"):                         {},
		strings.Join([]string{"video.runway-gen-4.5.adobe", "runway-gen-4.5", "adobe", "firefly-runway-4.5", ""}, "\x00"):                    {},
		strings.Join([]string{"video.runway-gen-4-turbo.runway", "runway-gen-4-turbo", "runway", "runway-gen4-turbo", "gen4_turbo"}, "\x00"): {},
		strings.Join([]string{"video.luma-ray.adobe", "luma-ray", "adobe", "firefly-ray", ""}, "\x00"):                                       {},
	}
	for _, match := range matches {
		key := strings.Join(match[1:], "\x00")
		if _, ok := want[key]; ok {
			delete(want, key)
			continue
		}
		if _, ok := retired[key]; ok {
			delete(retired, key)
			continue
		}
		t.Fatalf("SQL canonical route tuple is neither current nor retired: %q", match[0])
	}
	if len(want) != 0 {
		t.Fatalf("SQL canonical route allowlist is missing %d Go catalog routes", len(want))
	}
	if len(retired) != 0 {
		t.Fatalf("historical SQL allowlist is missing %d routes retired by migration 000005", len(retired))
	}
	if strings.Contains(sql, "route.enabled = TRUE") {
		t.Fatal("canonical route allowlist must not classify disabled routes as invalid")
	}
}
