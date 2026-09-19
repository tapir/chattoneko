package db

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestMigrateCreatesAllTables(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if err := Migrate(s); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rows, err := s.Query("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	t.Logf("tables: %v", names)
	want := []string{"attachments", "chats", "config", "message_attachments", "messages", "models", "schema_migrations", "tool_calls"}
	got := strings.Join(names, ",")
	for _, w := range want {
		if !strings.Contains(","+got+",", ","+w+",") {
			t.Fatalf("missing table %q: got %v", w, names)
		}
	}
}

func TestMigrateIdempotent(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if err := Migrate(s); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := Migrate(s); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

// The secondary indexes cover the hot paths: chat-load attachment metas and
// startup recovery of stuck generations.
func TestMigrateCreatesSecondaryIndexes(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if err := Migrate(s); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, idx := range []string{"idx_attachments_chat", "idx_messages_generating"} {
		var n int
		if err := s.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", idx).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("index %q missing after migrate", idx)
		}
	}
}

// Non-.sql files in the migrations directory are never executed as SQL.
func TestMigrateIgnoresNonSQLFiles(t *testing.T) {
	s, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	fsys := fstest.MapFS{
		"migrations/001_ok.sql": &fstest.MapFile{Data: []byte(
			"CREATE TABLE t (id INTEGER NOT NULL);")},
		"migrations/README.md":     &fstest.MapFile{Data: []byte("not SQL at all")},
		"migrations/002_notes.txt": &fstest.MapFile{Data: []byte("also not SQL")},
		"migrations/sub/003_x.sql": &fstest.MapFile{Data: []byte("ignored: not a direct child")},
	}
	if err := migrateFS(s, fsys); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var n int
	if err := s.QueryRow("SELECT COUNT(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("the .sql migration did not apply: %v", err)
	}
	if err := migrateFS(s, fsys); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

// TestMigrationFailureRollsBack: a migration whose second statement fails
// leaves no trace — no table, no schema_migrations row — so a corrected file
// re-applies cleanly on the next run.
func TestMigrationFailureRollsBack(t *testing.T) {
	s, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	bad := fstest.MapFS{
		"migrations/001_bad.sql": &fstest.MapFile{Data: []byte(
			"CREATE TABLE half_applied (id INTEGER NOT NULL);\nBROKEN SQL HERE;\n")},
	}
	if err := migrateFS(s, bad); err == nil {
		t.Fatal("expected migration failure")
	}
	var n int
	if err := s.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='half_applied'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("failed migration left its table behind")
	}
	if err := s.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("failed migration was recorded")
	}

	good := fstest.MapFS{
		"migrations/001_bad.sql": &fstest.MapFile{Data: []byte(
			"CREATE TABLE half_applied (id INTEGER NOT NULL);\nINSERT INTO half_applied (id) VALUES (1);\n")},
	}
	if err := migrateFS(s, good); err != nil {
		t.Fatalf("re-apply fixed migration: %v", err)
	}
	if err := s.QueryRow("SELECT COUNT(*) FROM half_applied").Scan(&n); err != nil || n != 1 {
		t.Fatalf("re-applied migration content: n=%d err=%v", n, err)
	}
}
