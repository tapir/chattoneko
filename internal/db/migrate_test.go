package db

import (
	"io/fs"
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

// The secondary indexes cover the hot paths: attachments by chat
// (chat-load metas) and the partial generating-messages
// index (startup recovery of stuck generations).
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

// TestMigrateUpgradesAttachmentLinks: a database written before 004 keeps its
// data — every message_id becomes a join-table row and the column itself is
// gone, so an upgrade never drops a file the user can still see.
func TestMigrateUpgradesAttachmentLinks(t *testing.T) {
	s, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// Everything but 004, straight from the embedded migrations.
	old := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "004_attachment_links.sql" {
			continue
		}
		b, err := fs.ReadFile(migrationsFS, "migrations/"+e.Name())
		if err != nil {
			t.Fatal(err)
		}
		old["migrations/"+e.Name()] = &fstest.MapFile{Data: b}
	}
	if err := migrateFS(s, old); err != nil {
		t.Fatalf("migrate to 003: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO chats (id, title, created_at, updated_at) VALUES ('c1', 'x', 1, 1)`,
		`INSERT INTO messages (id, chat_id, role, created_at, updated_at) VALUES ('m1', 'c1', 'user', 1, 1)`,
		`INSERT INTO attachments (id, chat_id, message_id, filename, kind, mime, size, data, created_at)
		 VALUES ('a1', 'c1', 'm1', 'cat.png', 'image', 'image/png', 3, 'png', 1)`,
		`INSERT INTO attachments (id, chat_id, message_id, filename, kind, mime, size, data, created_at)
		 VALUES ('a2', 'c1', '', 'staged.png', 'image', 'image/png', 3, 'png', 1)`,
	} {
		if _, err := s.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	if err := Migrate(s); err != nil {
		t.Fatalf("migrate 004: %v", err)
	}
	var msgID string
	if err := s.QueryRow(`SELECT message_id FROM message_attachments WHERE attachment_id='a1'`).Scan(&msgID); err != nil {
		t.Fatalf("link not carried over: %v", err)
	}
	if msgID != "m1" {
		t.Fatalf("link = %q, want m1", msgID)
	}
	var n int
	if err := s.QueryRow(`SELECT COUNT(*) FROM message_attachments WHERE attachment_id='a2'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("the staged orphan gained a link")
	}
	if err := s.QueryRow(`SELECT message_id FROM attachments LIMIT 1`).Scan(&msgID); err == nil {
		t.Fatal("attachments.message_id survived the migration")
	}
	// Deleting the message cascades the link away, leaving an orphan blob.
	if _, err := s.Exec(`DELETE FROM messages WHERE id='m1'`); err != nil {
		t.Fatalf("delete message: %v", err)
	}
	if err := s.QueryRow(`SELECT COUNT(*) FROM message_attachments`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("links left after message delete = %d, want 0", n)
	}
	if err := s.QueryRow(`SELECT COUNT(*) FROM attachments WHERE id='a1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("cascade took the blob with it (n=%d, err=%v)", n, err)
	}
}

// Non-.sql files in the migrations directory (READMEs, editor droppings)
// are ignored, never executed as SQL.
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

// TestMigrationFailureRollsBack verifies the transactional guarantee: a
// migration whose second statement fails leaves NO trace — the table from
// the first statement is rolled back and nothing is recorded — so a fixed
// file can be cleanly re-applied on the next run.
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
	// Rolled back: no table, no schema_migrations record.
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

	// A corrected file applies cleanly on retry.
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
