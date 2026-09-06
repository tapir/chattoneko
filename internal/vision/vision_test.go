package vision

import (
	"context"
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
