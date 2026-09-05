package vision

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"chattoneko/internal/config"
	"chattoneko/internal/db"
)

func newConfigStore(t *testing.T, c config.Config) *config.Store {
	t.Helper()
	sqlDB, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	cfgs, err := config.TestStore(context.Background(), sqlDB, c)
	if err != nil {
		t.Fatalf("config store: %v", err)
	}
	return cfgs
}

func TestServiceNotConfigured(t *testing.T) {
	cfgs := newConfigStore(t, config.Config{})
	s := New(cfgs)
	if _, err := s.DescribeImage(t.Context(), []byte("png"), "cat.png"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestServiceDescribe(t *testing.T) {
	var sawImageURL bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sawImageURL = strings.Contains(string(body), "data:image/png;base64,")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id": "x", "object": "chat.completion", "created": 0, "model": "v",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "a fluffy cat"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
		}`)
	}))
	defer srv.Close()

	cfgs := newConfigStore(t, config.Config{
		Provider: config.ProviderConfig{BaseURL: srv.URL, APIKey: "sk-test"},
		Models:   config.ModelsConfig{Whitelist: []string{"v"}, DefaultVisionModel: "v"},
	})
	s := New(cfgs)

	desc, err := s.DescribeImage(t.Context(), []byte{0x89, 'P', 'N', 'G'}, "cat.png")
	if err != nil {
		t.Fatalf("DescribeImage: %v", err)
	}
	if desc != "a fluffy cat" {
		t.Errorf("description = %q", desc)
	}
	if !sawImageURL {
		t.Error("request did not carry the image data URL")
	}
}

// The description is model output: control characters must be stripped
// before it reaches the chat prompt, the description endpoint or the chat
// log (newlines/tabs are legitimate description formatting and stay).
func TestServiceDescribeStripsControlChars(t *testing.T) {
	withServer := func(t *testing.T, content string) string {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}]}`, jsonString(content))
		}))
		defer srv.Close()
		cfgs := newConfigStore(t, config.Config{
			Provider: config.ProviderConfig{BaseURL: srv.URL, APIKey: "sk-test"},
			Models:   config.ModelsConfig{Whitelist: []string{"v"}, DefaultVisionModel: "v"},
		})
		desc, err := New(cfgs).DescribeImage(t.Context(), []byte{0x89, 'P', 'N', 'G'}, "cat.png")
		if err != nil {
			t.Fatalf("DescribeImage: %v", err)
		}
		return desc
	}

	if got := withServer(t, "a\x00b\x07c\nd\te"); got != "abc\nd\te" {
		t.Fatalf("description = %q, want control chars stripped and \\n/\\t kept", got)
	}
	// An all-control description collapses to empty, which the engine treats
	// as "no description" and falls back to sending the image.
	if got := withServer(t, "\x00\x1b\x07"); got != "" {
		t.Fatalf("all-control description = %q, want empty", got)
	}
}

// Filenames are external input interpolated into the prompt: control
// characters (e.g. percent-decoded URL path segments) must not reach it.
func TestServiceDescribeSanitizesFilenameInPrompt(t *testing.T) {
	var sawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	cfgs := newConfigStore(t, config.Config{
		Provider: config.ProviderConfig{BaseURL: srv.URL, APIKey: "sk-test"},
		Models:   config.ModelsConfig{Whitelist: []string{"v"}, DefaultVisionModel: "v"},
	})
	if _, err := New(cfgs).DescribeImage(t.Context(), []byte{0x89, 'P', 'N', 'G'}, "ca\x00t\x07.png"); err != nil {
		t.Fatalf("DescribeImage: %v", err)
	}
	body := string(sawBody)
	if !strings.Contains(body, "cat.png") {
		t.Fatalf("sanitized filename missing from prompt: %s", body)
	}
	if strings.Contains(body, "\\u0000") || strings.Contains(body, "\\u0007") {
		t.Fatalf("control characters leaked into the prompt: %s", body)
	}
}

// jsonString renders s as a JSON string literal for building fake responses.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestServiceClientRebuiltOnModelChange(t *testing.T) {
	var seenModels []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		seenModels = append(seenModels, req.Model)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	cfgs := newConfigStore(t, config.Config{
		Provider: config.ProviderConfig{BaseURL: srv.URL, APIKey: "sk-test"},
		Models:   config.ModelsConfig{Whitelist: []string{"v1", "v2"}, DefaultVisionModel: "v1"},
	})
	s := New(cfgs)

	if _, err := s.DescribeImage(t.Context(), []byte("png"), "a.png"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := cfgs.Update(context.Background(), config.Patch{
		Models: &config.ModelsPatch{
			Whitelist:          &[]string{"v1", "v2"},
			DefaultVisionModel: ptr("v2"),
		},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := s.DescribeImage(t.Context(), []byte("png"), "b.png"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if len(seenModels) != 2 || seenModels[0] != "v1" || seenModels[1] != "v2" {
		t.Errorf("models seen = %v, want [v1 v2]", seenModels)
	}
}

func ptr[T any](v T) *T { return &v }

// ---- vision client (reasoning effort from the models table) ----

// visionConfig builds a config store with provider + models configured and,
// when meta is non-nil, a stored metadata row for the vision model. The raw
// DB is returned alongside for tests that need to break it.
func visionConfig(t *testing.T, visionModel string, meta *config.ModelMeta) (*config.Store, *sql.DB) {
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
		Models: &config.ModelsPatch{
			Whitelist:          &[]string{visionModel},
			DefaultVisionModel: ptr(visionModel),
		},
	}
	if meta != nil {
		patch.Models.Metas = &[]config.ModelMeta{*meta}
	}
	if _, err := cfgs.Update(ctx, patch); err != nil {
		t.Fatalf("update config: %v", err)
	}
	return cfgs, sqlDB
}

func TestVisionClientUsesDatabaseDefaultEffort(t *testing.T) {
	meta := config.DefaultModelMeta("vision-m")
	meta.ReasoningEfforts = []string{"low", "high"}
	meta.ReasoningDefault = "high"
	cfgs, _ := visionConfig(t, "vision-m", &meta)

	svc := New(cfgs)
	cli := svc.client(context.Background())
	if cli == nil {
		t.Fatal("vision client nil despite provider/vision model configured")
	}
	if cli.effort != "high" {
		t.Fatalf("effort = %q, want the stored default %q", cli.effort, "high")
	}
}

func TestVisionClientDefaultEffortWithoutStoredRow(t *testing.T) {
	// No metadata row: ModelMetas applies the spec defaults (default medium).
	cfgs, _ := visionConfig(t, "vision-m", nil)

	svc := New(cfgs)
	cli := svc.client(context.Background())
	if cli == nil {
		t.Fatal("vision client nil")
	}
	if cli.effort != config.DefaultModelMeta("vision-m").ReasoningDefault {
		t.Fatalf("effort = %q, want spec default", cli.effort)
	}
}

func TestVisionClientRebuildsOnEffortChange(t *testing.T) {
	meta := config.DefaultModelMeta("vision-m")
	meta.ReasoningDefault = "low"
	cfgs, _ := visionConfig(t, "vision-m", &meta)

	svc := New(cfgs)
	ctx := context.Background()
	first := svc.client(ctx)
	if first == nil || first.effort != "low" {
		t.Fatalf("first client = %+v", first)
	}
	// Same settings: the cached client is reused.
	if got := svc.client(ctx); got != first {
		t.Fatal("client rebuilt without config change")
	}
	// Admin changes the stored default effort: the client must be rebuilt.
	meta.ReasoningDefault = "high"
	if err := cfgs.UpsertModelMetas(ctx, []config.ModelMeta{meta}); err != nil {
		t.Fatalf("upsert metas: %v", err)
	}
	second := svc.client(ctx)
	if second == first {
		t.Fatal("client not rebuilt after effort change")
	}
	if second.effort != "high" {
		t.Fatalf("effort = %q, want high", second.effort)
	}
}

func TestVisionClientFallsBackOnMetaLoadFailure(t *testing.T) {
	meta := config.DefaultModelMeta("vision-m")
	cfgs, sqlDB := visionConfig(t, "vision-m", &meta)

	svc := New(cfgs)
	// Break the models table; metadata reads fail but descriptions must not stall.
	if _, err := sqlDB.Exec(`DROP TABLE models`); err != nil {
		t.Fatalf("drop models table: %v", err)
	}
	cli := svc.client(context.Background())
	if cli == nil {
		t.Fatal("vision client nil on metadata failure; want provider-default fallback")
	}
	if cli.effort != "" {
		t.Fatalf("effort = %q, want empty (provider default)", cli.effort)
	}
}

func TestServiceSendsReasoningEffort(t *testing.T) {
	var sawEffort string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ReasoningEffort string `json:"reasoning_effort"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		sawEffort = req.ReasoningEffort
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	meta := config.DefaultModelMeta("v")
	meta.ReasoningDefault = "low"
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
	if _, err := cfgs.Update(ctx, config.Patch{
		Provider: &config.ProviderPatch{BaseURL: ptr(srv.URL), APIKey: ptr("sk-test")},
		Models: &config.ModelsPatch{
			Whitelist:          &[]string{"v"},
			DefaultVisionModel: ptr("v"),
			Metas:              &[]config.ModelMeta{meta},
		},
	}); err != nil {
		t.Fatalf("update config: %v", err)
	}

	s := New(cfgs)
	if _, err := s.DescribeImage(t.Context(), []byte{0x89, 'P', 'N', 'G'}, "cat.png"); err != nil {
		t.Fatalf("DescribeImage: %v", err)
	}
	if sawEffort != "low" {
		t.Errorf("reasoning_effort = %q, want %q", sawEffort, "low")
	}
}
