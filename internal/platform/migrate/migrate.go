// Package migrate implements a small, dependency-free migration runner over
// the embedded SQL files. Migrations are applied in version order inside a
// transaction, guarded by a PostgreSQL advisory lock so that concurrent
// application instances cannot race.
package migrate

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

var fileNamePattern = regexp.MustCompile(`^(\d+)_([A-Za-z0-9_-]+)\.(up|down)\.sql$`)

// Migration is a single versioned schema change with an up and a down script.
type Migration struct {
	Version int64
	Name    string
	UpSQL   string
	DownSQL string
}

// Load reads and parses all migrations from the given file system.
func Load(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("migrate: read dir: %w", err)
	}

	byVersion := map[int64]*Migration{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		matches := fileNamePattern.FindStringSubmatch(name)
		if matches == nil {
			continue
		}
		version, err := strconv.ParseInt(matches[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migrate: invalid version in %q: %w", name, err)
		}
		content, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("migrate: read %q: %w", name, err)
		}
		m, ok := byVersion[version]
		if !ok {
			m = &Migration{Version: version, Name: matches[2]}
			byVersion[version] = m
		}
		if m.Name != matches[2] {
			return nil, fmt.Errorf("migrate: mismatched name for version %d (%q vs %q)", version, m.Name, matches[2])
		}
		switch matches[3] {
		case "up":
			m.UpSQL = string(content)
		case "down":
			m.DownSQL = string(content)
		}
	}

	migrations := make([]Migration, 0, len(byVersion))
	for _, m := range byVersion {
		if strings.TrimSpace(m.UpSQL) == "" {
			return nil, fmt.Errorf("migrate: version %d is missing the up script", m.Version)
		}
		if strings.TrimSpace(m.DownSQL) == "" {
			return nil, fmt.Errorf("migrate: version %d is missing the down script", m.Version)
		}
		migrations = append(migrations, *m)
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })

	for i := 1; i < len(migrations); i++ {
		if migrations[i].Version == migrations[i-1].Version {
			return nil, fmt.Errorf("migrate: duplicate version %d", migrations[i].Version)
		}
	}
	return migrations, nil
}

const (
	createVersionTable = `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    BIGINT PRIMARY KEY,
			name       TEXT        NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`
	advisoryLockKey = 7320230401
)

// Up applies every migration newer than the current schema version.
// It returns the number of migrations applied.
func Up(ctx context.Context, pool *pgxpool.Pool, migrations []Migration) (int, error) {
	if _, err := pool.Exec(ctx, createVersionTable); err != nil {
		return 0, fmt.Errorf("migrate: create version table: %w", err)
	}
	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, m := range migrations {
		if _, ok := applied[m.Version]; ok {
			continue
		}
		if err := applyOne(ctx, pool, m, true); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// Down reverts the most recently applied migrations. steps <= 0 reverts all.
// It returns the number of migrations reverted.
func Down(ctx context.Context, pool *pgxpool.Pool, migrations []Migration, steps int) (int, error) {
	if _, err := pool.Exec(ctx, createVersionTable); err != nil {
		return 0, fmt.Errorf("migrate: create version table: %w", err)
	}
	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return 0, err
	}

	byVersion := make(map[int64]Migration, len(migrations))
	for _, m := range migrations {
		byVersion[m.Version] = m
	}

	versions := make([]int64, 0, len(applied))
	for v := range applied {
		versions = append(versions, v)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] > versions[j] })

	if steps > 0 && steps < len(versions) {
		versions = versions[:steps]
	}

	count := 0
	for _, version := range versions {
		m, ok := byVersion[version]
		if !ok {
			return count, fmt.Errorf("migrate: no down script for applied version %d", version)
		}
		if err := applyOne(ctx, pool, m, false); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// CurrentVersion returns the latest applied version (0 when the schema is empty).
func CurrentVersion(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	if _, err := pool.Exec(ctx, createVersionTable); err != nil {
		return 0, fmt.Errorf("migrate: create version table: %w", err)
	}
	var version int64
	err := pool.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("migrate: current version: %w", err)
	}
	return version, nil
}

func appliedVersions(ctx context.Context, pool *pgxpool.Pool) (map[int64]struct{}, error) {
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("migrate: list applied: %w", err)
	}
	defer rows.Close()

	applied := map[int64]struct{}{}
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("migrate: scan applied: %w", err)
		}
		applied[version] = struct{}{}
	}
	return applied, rows.Err()
}

func applyOne(ctx context.Context, pool *pgxpool.Pool, m Migration, up bool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrate: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, advisoryLockKey); err != nil {
		return fmt.Errorf("migrate: acquire lock: %w", err)
	}

	direction := "down"
	script := m.DownSQL
	if up {
		direction = "up"
		script = m.UpSQL
	}
	if _, err := tx.Exec(ctx, script); err != nil {
		return fmt.Errorf("migrate: apply %s %d_%s: %w", direction, m.Version, m.Name, err)
	}

	if up {
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.Version, m.Name); err != nil {
			return fmt.Errorf("migrate: record version %d: %w", m.Version, err)
		}
	} else {
		if _, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, m.Version); err != nil {
			return fmt.Errorf("migrate: delete version %d: %w", m.Version, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migrate: commit %s %d_%s: %w", direction, m.Version, m.Name, err)
	}
	return nil
}

// FileName returns the conventional file name for a migration script.
func FileName(m Migration, up bool) string {
	direction := "down"
	if up {
		direction = "up"
	}
	return path.Join("migrations", fmt.Sprintf("%04d_%s.%s.sql", m.Version, m.Name, direction))
}
