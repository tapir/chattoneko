package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"chattoneko/internal/mcphub"
)

func TestRegistryTools(t *testing.T) {
	r := New(Tool{
		Name:           "alpha",
		Description:    "first",
		DefaultEnabled: true,
		Handler:        func(context.Context, string, mcphub.CallMeta) (string, error) { return "ok", nil },
	})
	entries := r.Tools()
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Display != "alpha" || e.Description != "first" || e.Server != serverLabel || !e.DefaultEnabled {
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
	New(Tool{Name: "x", Handler: h}, Tool{Name: "x", Handler: h})
}

func TestRegistryCall(t *testing.T) {
	r := New(Tool{
		Name:    "echo",
		Handler: func(_ context.Context, args string, _ mcphub.CallMeta) (string, error) { return "got:" + args, nil },
	}, Tool{
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
	r := New(Tool{
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

// mustParseRFC extracts the RFC 3339 timestamp from a time_location result.
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

func TestTimeLocation(t *testing.T) {
	t.Run("without location", func(t *testing.T) {
		t.Setenv(EnvLocationString, "")
		out, isErr, err := Builtin(nil, nil).Call(context.Background(), "time_location", "", mcphub.CallMeta{})
		if err != nil || isErr {
			t.Fatalf("time_location: out=%q isErr=%v err=%v", out, isErr, err)
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
		t.Setenv(EnvLocationString, loc)
		out, isErr, err := Builtin(nil, nil).Call(context.Background(), "time_location", "", mcphub.CallMeta{})
		if err != nil || isErr {
			t.Fatalf("time_location: out=%q isErr=%v err=%v", out, isErr, err)
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
	mk := func(name, server string) Source {
		return &stubSource{
			entries: []mcphub.Entry{{Display: name, Server: server}},
			out:     "from-" + server,
		}
	}
	m := Merge(mk("time_location", "builtin"), mk("time_location", "mcp-a"), mk("search", "mcp-a"))

	entries := m.Tools()
	if len(entries) != 2 {
		t.Fatalf("want 2 deduplicated entries, got %d", len(entries))
	}

	// Collision: the FIRST source (builtin) owns the name.
	out, _, err := m.Call(context.Background(), "time_location", "", mcphub.CallMeta{})
	if err != nil || out != "from-builtin" {
		t.Fatalf("time_location routed wrong: out=%q err=%v", out, err)
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
