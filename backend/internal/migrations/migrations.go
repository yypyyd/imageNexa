package migrations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

const migrationAdvisoryLock int64 = 0x324150494D494752 // "2APIMIGR"

var migrationFilePattern = regexp.MustCompile(`^(\d{6})_([a-z0-9_]+)\.sql$`)

//go:embed sql/*.sql
var migrationFiles embed.FS

type Migration struct {
	Version  int64
	Name     string
	Filename string
	SQL      string
	Checksum string
}

type AppliedMigration struct {
	Version   int64
	Name      string
	Checksum  string
	AppliedAt time.Time
}

func Load() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "sql")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	migrations := make([]Migration, 0, len(entries))
	seen := make(map[int64]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filename := filepath.ToSlash(entry.Name())
		matches := migrationFilePattern.FindStringSubmatch(filename)
		if matches == nil {
			return nil, fmt.Errorf("invalid migration filename %q", filename)
		}
		version, err := strconv.ParseInt(matches[1], 10, 64)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("invalid migration version in %q", filename)
		}
		if previous, exists := seen[version]; exists {
			return nil, fmt.Errorf("duplicate migration version %d in %q and %q", version, previous, filename)
		}
		seen[version] = filename

		raw, err := migrationFiles.ReadFile("sql/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", filename, err)
		}
		if strings.TrimSpace(string(raw)) == "" {
			return nil, fmt.Errorf("migration %q is empty", filename)
		}
		digest := sha256.Sum256(raw)
		migrations = append(migrations, Migration{
			Version:  version,
			Name:     matches[2],
			Filename: filename,
			SQL:      string(raw),
			Checksum: hex.EncodeToString(digest[:]),
		})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	return migrations, nil
}

// Run applies every not-yet-recorded migration exactly once. A dedicated SQL
// connection owns the session-level advisory lock so it cannot accidentally be
// released on a different pooled connection.
func Run(ctx context.Context, db *gorm.DB) error {
	if db == nil {
		return errors.New("migration database is required")
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("resolve migration database: %w", err)
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version BIGINT PRIMARY KEY,
			name TEXT NOT NULL,
			checksum CHAR(64) NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationAdvisoryLock); err != nil {
		return fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", migrationAdvisoryLock)
	}()

	migrations, err := Load()
	if err != nil {
		return err
	}
	applied, err := readApplied(ctx, conn)
	if err != nil {
		return err
	}
	known := make(map[int64]Migration, len(migrations))
	for _, migration := range migrations {
		known[migration.Version] = migration
	}
	for version, record := range applied {
		migration, exists := known[version]
		if !exists {
			return fmt.Errorf("database contains unknown migration version %d (%s)", version, record.Name)
		}
		if record.Name != migration.Name || record.Checksum != migration.Checksum {
			return fmt.Errorf("migration %06d checksum/name mismatch", version)
		}
	}

	for _, migration := range migrations {
		if _, exists := applied[migration.Version]; exists {
			continue
		}
		if err := applyOne(ctx, conn, migration); err != nil {
			return err
		}
	}
	return nil
}

func CurrentVersion(ctx context.Context, db *gorm.DB) (int64, error) {
	if db == nil {
		return 0, errors.New("migration database is required")
	}
	var version sql.NullInt64
	err := db.WithContext(ctx).Raw("SELECT MAX(version) FROM schema_migrations").Scan(&version).Error
	if err != nil {
		return 0, err
	}
	if !version.Valid {
		return 0, nil
	}
	return version.Int64, nil
}

func readApplied(ctx context.Context, conn *sql.Conn) (map[int64]AppliedMigration, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT version, name, checksum, applied_at
		FROM schema_migrations
		ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int64]AppliedMigration)
	for rows.Next() {
		var record AppliedMigration
		if err := rows.Scan(&record.Version, &record.Name, &record.Checksum, &record.AppliedAt); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		applied[record.Version] = record
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied migrations: %w", err)
	}
	return applied, nil
}

func applyOne(ctx context.Context, conn *sql.Conn, migration Migration) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %06d: %w", migration.Version, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
		return fmt.Errorf("apply migration %06d_%s: %w", migration.Version, migration.Name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)",
		migration.Version, migration.Name, migration.Checksum,
	); err != nil {
		return fmt.Errorf("record migration %06d: %w", migration.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %06d: %w", migration.Version, err)
	}
	return nil
}
