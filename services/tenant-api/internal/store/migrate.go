// Package store wraps the Postgres-backed tenant control-plane data
// access for the tenant-api service.
//
// Migrations are intentionally implemented in-package, with no external
// migration tool, so the deploy story is "ship a single binary, run
// it, and it brings the schema up to date." The migrator is small
// enough to audit in one sitting and big enough to be safe:
//
//   - Each migration runs inside a transaction.
//   - schema_migrations is the source of truth for what's applied.
//   - File-name lexicographic order determines apply order.
//   - Already-applied migrations are skipped.
//
// To add a migration: drop a new file in migrations/NNN_description.sql
// with NNN strictly greater than the last applied version. Migrations
// are append-only; do not rewrite history.
package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ApplyMigrations brings the database schema up to date. Safe to call
// at every service start; it is a no-op when nothing is pending.
func ApplyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("store: bootstrap schema_migrations: %w", err)
	}

	applied, err := loadApplied(ctx, pool)
	if err != nil {
		return err
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("store: read embedded migrations: %w", err)
	}

	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	for _, name := range files {
		version := strings.TrimSuffix(name, ".sql")
		if applied[version] {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("store: read migration %s: %w", name, err)
		}
		if err := applyOne(ctx, pool, version, string(body)); err != nil {
			return fmt.Errorf("store: apply %s: %w", name, err)
		}
	}
	return nil
}

func loadApplied(ctx context.Context, pool *pgxpool.Pool) (map[string]bool, error) {
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: load applied: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

func applyOne(ctx context.Context, pool *pgxpool.Pool, version, body string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, body); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO schema_migrations (version) VALUES ($1)
	`, version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
