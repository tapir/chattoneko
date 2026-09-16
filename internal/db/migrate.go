// Package db embeds and applies the SQL migrations at startup.
package db

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies every migration not yet recorded in schema_migrations, in
// lexical filename order. SQLite DDL is transactional: each file runs in one
// transaction together with its schema_migrations row, so a mid-file failure
// rolls back completely and a retry starts clean.
func Migrate(db *sql.DB) error {
	return migrateFS(db, migrationsFS)
}

// migrateFS applies migrations from fsys (separated for tests).
func migrateFS(db *sql.DB, fsys fs.FS) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		filename TEXT NOT NULL PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("bootstrap schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var applied int64
		row := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE filename = ?`, name)
		if err := row.Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if applied > 0 {
			continue
		}
		raw, err := fs.ReadFile(fsys, "migrations/"+name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err := tx.Exec(string(raw)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (filename, applied_at) VALUES (?, ?)`,
			name, time.Now().UnixMilli()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}
