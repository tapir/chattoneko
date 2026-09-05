package tools

import (
	"context"
	"testing"

	"chattoneko/internal/config"
	"chattoneko/internal/db"
	"chattoneko/internal/mcphub"
)

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
	for _, e := range WithDefaults(src, cfg).Tools() {
		got[e.Display] = e.DefaultEnabled
	}
	if got["on_tool"] != true || got["off_tool"] != false || got["untouched"] != true {
		t.Fatalf("defaults = %v, want on_tool=true off_tool=false untouched=true", got)
	}

	// The map is read live, so a settings save applies without a rebuild.
	if _, err := cfg.Update(ctx, config.Patch{ToolDefaults: &map[string]bool{"untouched": false}}); err != nil {
		t.Fatalf("update: %v", err)
	}
	for _, e := range WithDefaults(src, cfg).Tools() {
		if e.Display == "untouched" && e.DefaultEnabled {
			t.Fatal("untouched should follow the updated config")
		}
	}
}
