package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"chattoneko/internal/config"
	"chattoneko/internal/db"
	"chattoneko/internal/mcphub"
)

func TestRegistryTools(t *testing.T) {
	r := newRegistry(tool{
		Name:           "alpha",
		Description:    "first",
		DefaultEnabled: true,
		Title:          "Alphing…",
		Handler:        func(context.Context, string, mcphub.CallMeta) (string, error) { return "ok", nil },
	})
	entries := r.Tools()
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Display != "alpha" || e.Description != "first" || e.Server != mcphub.BuiltinServer || !e.DefaultEnabled || e.Title != "Alphing…" {
		t.Fatalf("unexpected entry: %+v", e)
	}
	// A tool without a schema gets the empty-object default.
	if string(e.Schema) != `{"type":"object","properties":{}}` {
		t.Fatalf("unexpected default schema: %s", e.Schema)
	}
}

func TestRegistryDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate tool name")
		}
	}()
	h := func(context.Context, string, mcphub.CallMeta) (string, error) { return "", nil }
	newRegistry(tool{Name: "x", Handler: h}, tool{Name: "x", Handler: h})
}

func TestRegistryCall(t *testing.T) {
	r := newRegistry(tool{
		Name:    "echo",
		Handler: func(_ context.Context, args string, _ mcphub.CallMeta) (string, error) { return "got:" + args, nil },
	}, tool{
		Name:    "fail",
		Handler: func(context.Context, string, mcphub.CallMeta) (string, error) { return "", errors.New("boom") },
	})

	out, isErr, err := r.Call(context.Background(), "echo", `{"a":1}`, mcphub.CallMeta{})
	if err != nil || isErr || out != `got:{"a":1}` {
		t.Fatalf("echo: out=%q isErr=%v err=%v", out, isErr, err)
	}

	// Handler errors surface in-band (isError=true), like MCP tool errors.
	out, isErr, err = r.Call(context.Background(), "fail", "", mcphub.CallMeta{})
	if err != nil || !isErr || out != "Error: boom" {
		t.Fatalf("fail: out=%q isErr=%v err=%v", out, isErr, err)
	}

	if _, _, err := r.Call(context.Background(), "nope", "", mcphub.CallMeta{}); err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

// A panicking in-process handler (e.g. a bug in the Lua VM) must surface as
// an in-band tool error, never take down the turn loop or the process.
func TestRegistryCallPanicIsolated(t *testing.T) {
	r := newRegistry(tool{
		Name:    "explode",
		Handler: func(context.Context, string, mcphub.CallMeta) (string, error) { panic("boom") },
	})
	out, isErr, err := r.Call(context.Background(), "explode", "", mcphub.CallMeta{})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !isErr || !strings.Contains(out, "internal tool failure") {
		t.Fatalf("panic not converted to an in-band error: out=%q isErr=%v", out, isErr)
	}
}

// mustParseRFC extracts the RFC 3339 timestamp from a time result.
// It is the only parenthesised group, so this works whether or not a location
// suffix follows it.
func mustParseRFC(t *testing.T, out string) time.Time {
	t.Helper()
	open := strings.Index(out, "(")
	close := strings.Index(out, ")")
	if open < 0 || close < 0 {
		t.Fatalf("unexpected shape: %q", out)
	}
	ts, err := time.Parse(time.RFC3339, out[open+1:close])
	if err != nil {
		t.Fatalf("rfc3339 part does not parse: %q (%v)", out, err)
	}
	return ts
}

// setLocation points the time tool at loc for one test: the env var is read
// once at startup, so the stored value is what a test has to change.
func setLocation(t *testing.T, loc string) {
	t.Helper()
	old := locationString
	locationString = loc
	t.Cleanup(func() { locationString = old })
}

func TestTime(t *testing.T) {
	t.Run("without location", func(t *testing.T) {
		setLocation(t, "")
		out, isErr, err := Builtin(nil, nil).Call(context.Background(), "time", "", mcphub.CallMeta{})
		if err != nil || isErr {
			t.Fatalf("time: out=%q isErr=%v err=%v", out, isErr, err)
		}
		// No location configured → the result ends at the RFC3339 paren.
		if !strings.HasSuffix(out, ")") {
			t.Fatalf("expected no location suffix: %q", out)
		}
		ts := mustParseRFC(t, out)
		if d := time.Since(ts); d < -time.Minute || d > time.Minute {
			t.Fatalf("returned time %v is not ~now (delta %v)", ts, d)
		}
	})

	t.Run("with location", func(t *testing.T) {
		const loc = "Berlin, Germany"
		setLocation(t, loc)
		out, isErr, err := Builtin(nil, nil).Call(context.Background(), "time", "", mcphub.CallMeta{})
		if err != nil || isErr {
			t.Fatalf("time: out=%q isErr=%v err=%v", out, isErr, err)
		}
		// Location configured → appended as " — <location>".
		if !strings.HasSuffix(out, " — "+loc) {
			t.Fatalf("expected location appended: %q", out)
		}
		ts := mustParseRFC(t, out)
		if d := time.Since(ts); d < -time.Minute || d > time.Minute {
			t.Fatalf("returned time %v is not ~now (delta %v)", ts, d)
		}
	})
}

func TestMerge(t *testing.T) {
	mk := func(name, server string) source {
		return &stubSource{
			entries: []mcphub.Entry{{Display: name, Server: server}},
			out:     "from-" + server,
		}
	}
	m := Merge(nil, mk("time", "builtin"), mk("time", "mcp-a"), mk("search", "mcp-a"))

	entries := m.Tools()
	if len(entries) != 2 {
		t.Fatalf("want 2 deduplicated entries, got %d", len(entries))
	}

	// Collision: the FIRST source (builtin) owns the name.
	out, _, err := m.Call(context.Background(), "time", "", mcphub.CallMeta{})
	if err != nil || out != "from-builtin" {
		t.Fatalf("time routed wrong: out=%q err=%v", out, err)
	}
	out, _, err = m.Call(context.Background(), "search", "", mcphub.CallMeta{})
	if err != nil || out != "from-mcp-a" {
		t.Fatalf("search routed wrong: out=%q err=%v", out, err)
	}
	if _, _, err := m.Call(context.Background(), "ghost", "", mcphub.CallMeta{}); err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

type stubSource struct {
	entries []mcphub.Entry
	out     string
}

func (s *stubSource) Tools() []mcphub.Entry { return s.entries }
func (s *stubSource) Call(context.Context, string, string, mcphub.CallMeta) (string, bool, error) {
	return s.out, false, nil
}

// A tool that needs a designated model is not in the catalog at all until one
// exists: the model is never offered a call that could only fail, and neither
// tool list shows a switch for it. Designating one brings its tool back live.
func TestCatalogHidesToolsWithoutAModel(t *testing.T) {
	sqlDB, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	cfgs, err := config.TestStore(ctx, sqlDB, config.Config{})
	if err != nil {
		t.Fatalf("config store: %v", err)
	}
	listed := func() map[string]bool {
		have := map[string]bool{}
		for _, e := range Builtin(&fakeFileStore{}, cfgs).Tools() {
			have[e.Display] = true
		}
		return have
	}

	have := listed()
	for _, name := range []string{"vision", "document", "transcription", "speak"} {
		if have[name] {
			t.Errorf("%s is listed with no model designated for it", name)
		}
	}
	for _, name := range []string{"time", "code", "create_file", "fetch"} {
		if !have[name] {
			t.Errorf("%s needs no designated model and went missing", name)
		}
	}

	// The audio ids are free-standing, so a save designates them with no
	// whitelist or metadata to satisfy.
	transcribe, speak, voice := "whisper-1", "tts-1", "alloy"
	if _, err := cfgs.Update(ctx, config.Patch{Models: &config.ModelsPatch{
		DefaultTranscriptionModel: &transcribe,
		DefaultSpeechModel:        &speak,
		SpeechVoice:               &voice,
	}}); err != nil {
		t.Fatalf("update: %v", err)
	}
	have = listed()
	if !have["transcription"] || !have["speak"] {
		t.Errorf("the audio designations did not bring their tools back: %v", have)
	}
	if have["vision"] || have["document"] {
		t.Errorf("a tool whose own role is still undesignated came back: %v", have)
	}
}
