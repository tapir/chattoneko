package llm

import (
	"context"
	"database/sql"
	"testing"

	"chattoneko/internal/config"
	"chattoneko/internal/db"
)

// configured builds a config store with a provider set and, when meta is
// non-nil, a stored metadata row for model. The raw DB comes back too so a
// test can break the models table.
func configured(t *testing.T, model string, meta *config.ModelMeta) (*config.Store, *sql.DB) {
	t.Helper()
	sqlDB, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	ctx := context.Background()
	cfgs, err := config.NewStore(ctx, sqlDB)
	if err != nil {
		t.Fatalf("config store: %v", err)
	}
	patch := config.Patch{
		Provider: &config.ProviderPatch{BaseURL: ptr("https://p.example/api"), APIKey: ptr("sk-test")},
		Models:   &config.ModelsPatch{Whitelist: &[]string{model}},
	}
	if meta != nil {
		patch.Models.Metas = &[]config.ModelMeta{*meta}
	}
	if _, err := cfgs.Update(ctx, patch); err != nil {
		t.Fatalf("update config: %v", err)
	}
	return cfgs, sqlDB
}

func ptr[T any](v T) *T { return &v }

// get resolves the client for the store's configured provider + model.
func get(t *testing.T, cfgs *config.Store, model string) *Client {
	t.Helper()
	return NewCache(cfgs).Get(context.Background(), model)
}

func TestUnconfiguredReturnsNil(t *testing.T) {
	// No designated model: nil without ever touching the store.
	if got := NewCache(nil).Get(context.Background(), ""); got != nil {
		t.Fatalf("Get(\"\") = %+v, want nil", got)
	}
	// Provider not configured yet (fresh install): nil.
	sqlDB, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	cfgs, err := config.NewStore(context.Background(), sqlDB)
	if err != nil {
		t.Fatalf("config store: %v", err)
	}
	if got := get(t, cfgs, "m"); got != nil {
		t.Fatalf("unconfigured provider: got %+v, want nil", got)
	}
}

func TestEffortFromStoredMetadata(t *testing.T) {
	meta := config.DefaultModelMeta("m")
	meta.ReasoningEfforts = []string{"low", "high"}
	meta.ReasoningDefault = "high"
	cfgs, _ := configured(t, "m", &meta)

	cli := get(t, cfgs, "m")
	if cli == nil {
		t.Fatal("client nil despite provider configured")
	}
	if cli.effort != "high" {
		t.Fatalf("effort = %q, want the stored default %q", cli.effort, "high")
	}
}

func TestEffortDefaultsWithoutStoredRow(t *testing.T) {
	cfgs, _ := configured(t, "m", nil)

	cli := get(t, cfgs, "m")
	if cli == nil {
		t.Fatal("client nil")
	}
	if cli.effort != config.DefaultModelMeta("m").ReasoningDefault {
		t.Fatalf("effort = %q, want spec default", cli.effort)
	}
}

func TestClientReusedThenRebuiltOnChange(t *testing.T) {
	meta := config.DefaultModelMeta("m")
	meta.ReasoningDefault = "low"
	cfgs, _ := configured(t, "m", &meta)

	c := NewCache(cfgs)
	ctx := context.Background()
	first := c.Get(ctx, "m")
	if first == nil || first.effort != "low" {
		t.Fatalf("first client = %+v", first)
	}
	// Same settings: the cached client (and its connection pool) is reused.
	if got := c.Get(ctx, "m"); got != first {
		t.Fatal("client rebuilt without config change")
	}
	// Admin changes the stored default effort: the client must be rebuilt.
	meta.ReasoningDefault = "high"
	if err := cfgs.UpsertModelMetas(ctx, []config.ModelMeta{meta}); err != nil {
		t.Fatalf("upsert metas: %v", err)
	}
	second := c.Get(ctx, "m")
	if second == first {
		t.Fatal("client not rebuilt after effort change")
	}
	if second.effort != "high" {
		t.Fatalf("effort = %q, want high", second.effort)
	}
	// A different model rebuilds too.
	if got := c.Get(ctx, "other"); got == second {
		t.Fatal("client not rebuilt for a different model")
	}
}

func TestEffortFallsBackOnMetaLoadFailure(t *testing.T) {
	meta := config.DefaultModelMeta("m")
	cfgs, sqlDB := configured(t, "m", &meta)

	// Break the models table; metadata reads fail but callers must not stall.
	if _, err := sqlDB.Exec(`DROP TABLE models`); err != nil {
		t.Fatalf("drop models table: %v", err)
	}
	cli := get(t, cfgs, "m")
	if cli == nil {
		t.Fatal("client nil on metadata failure; want provider-default fallback")
	}
	if cli.effort != "" {
		t.Fatalf("effort = %q, want empty (provider default)", cli.effort)
	}
}
