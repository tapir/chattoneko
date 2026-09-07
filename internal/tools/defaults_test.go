package tools

import (
	"context"
	"strings"
	"testing"

	"chattoneko/internal/config"
	"chattoneko/internal/db"
	"chattoneko/internal/mcphub"
)

// TestTitlesOverrideCatalogTitle: a configured title replaces the source's
// own, an absent or blank one keeps it, and a save applies live.
func TestTitlesOverrideCatalogTitle(t *testing.T) {
	sqlDB, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	cfg, err := config.TestStore(ctx, sqlDB, config.Config{
		ToolTitles: map[string]string{"overridden": "Searching web…", "blanked": "   "},
	})
	if err != nil {
		t.Fatalf("config store: %v", err)
	}
	src := &stubSource{entries: []mcphub.Entry{
		{Display: "overridden", Title: "server title"},
		{Display: "blanked", Title: "server title"},
		{Display: "untouched", Title: "server title"},
	}}

	titles := map[string]string{}
	for _, e := range Merge(cfg, src).Tools() {
		titles[e.Display] = e.Title
	}
	if titles["overridden"] != "Searching web…" {
		t.Fatalf("overridden = %q, want the configured title", titles["overridden"])
	}
	if titles["blanked"] != "server title" || titles["untouched"] != "server title" {
		t.Fatalf("titles = %v, want blanked/untouched to keep the source title", titles)
	}

	// The map is read live, so a settings save applies without a rebuild.
	if _, err := cfg.Update(ctx, config.Patch{ToolTitles: &map[string]string{"untouched": "Other…"}}); err != nil {
		t.Fatalf("update: %v", err)
	}
	for _, e := range Merge(cfg, src).Tools() {
		if e.Display == "untouched" && e.Title != "Other…" {
			t.Fatalf("untouched = %q, want it to follow the updated config", e.Title)
		}
	}
}

// TestBuiltinTitlesSet: every integrated tool carries a user-facing title, so
// the chat never falls back to a raw identifier like "simple_code".
func TestBuiltinTitlesSet(t *testing.T) {
	for _, e := range Builtin(nil, nil).Tools() {
		if strings.TrimSpace(e.Title) == "" {
			t.Errorf("integrated tool %q has no title", e.Display)
		}
	}
}

// TestDefaultsOverridesCatalogDefault: the configured global map wins over the
// source's own DefaultEnabled; tools absent from it keep theirs.
func TestDefaultsOverridesCatalogDefault(t *testing.T) {
	sqlDB, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	cfg, err := config.TestStore(ctx, sqlDB, config.Config{
		ToolDefaults: map[string]bool{"on_tool": true, "off_tool": false},
	})
	if err != nil {
		t.Fatalf("config store: %v", err)
	}
	src := &stubSource{entries: []mcphub.Entry{
		{Display: "on_tool", DefaultEnabled: false},
		{Display: "off_tool", DefaultEnabled: true},
		{Display: "untouched", DefaultEnabled: true},
	}}

	got := map[string]bool{}
	for _, e := range Merge(cfg, src).Tools() {
		got[e.Display] = e.DefaultEnabled
	}
	if got["on_tool"] != true || got["off_tool"] != false || got["untouched"] != true {
		t.Fatalf("defaults = %v, want on_tool=true off_tool=false untouched=true", got)
	}

	// The map is read live, so a settings save applies without a rebuild.
	if _, err := cfg.Update(ctx, config.Patch{ToolDefaults: &map[string]bool{"untouched": false}}); err != nil {
		t.Fatalf("update: %v", err)
	}
	for _, e := range Merge(cfg, src).Tools() {
		if e.Display == "untouched" && e.DefaultEnabled {
			t.Fatal("untouched should follow the updated config")
		}
	}
}
