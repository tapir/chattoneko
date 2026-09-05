package mcphub

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"chattoneko/internal/config"
	"chattoneko/internal/db"
)

var testServerBinary string

// testStore builds a config.Store over an in-memory DB seeded with cfg.
func testStore(t *testing.T, cfg config.Config) *config.Store {
	t.Helper()
	sqlDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := config.TestStore(context.Background(), sqlDB, cfg)
	if err != nil {
		t.Fatalf("test store: %v", err)
	}
	return st
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "testmcp")
	if err != nil {
		panic(err)
	}
	testServerBinary = filepath.Join(dir, "testmcp")
	cmd := exec.Command("go", "build", "-o", testServerBinary, "./testdata/testmcp")
	if out, err := cmd.CombinedOutput(); err != nil {
		panic("build testmcp: " + err.Error() + ": " + string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func testConfig() config.Config {
	return config.Config{
		MCPServers: []config.MCPServerConfig{
			{Name: "test", Transport: "stdio", Command: testServerBinary, DefaultEnabled: true},
		},
	}
}

func TestHubListAndCall(t *testing.T) {
	hub := New(testStore(t, testConfig()))
	hub.Connect(context.Background())
	defer hub.Close()

	tools := hub.Tools()
	if len(tools) != 2 {
		t.Fatalf("want 2 tools, got %d: %+v", len(tools), tools)
	}
	names := map[string]Entry{}
	for _, e := range tools {
		names[e.Display] = e
	}
	for _, want := range []string{"echo", "always_fails"} {
		if _, ok := names[want]; !ok {
			t.Fatalf("missing tool %q in %v", want, names)
		}
	}
	if names["echo"].Server != "test" || !names["echo"].DefaultEnabled {
		t.Fatalf("echo entry wrong: %+v", names["echo"])
	}
	if len(names["echo"].Schema) == 0 {
		t.Fatal("echo schema empty")
	}

	// successful call round-trip
	res, isError, err := hub.Call(context.Background(), "echo", `{"text":"hello mcp"}`, CallMeta{})
	if err != nil {
		t.Fatalf("call echo: %v", err)
	}
	if isError {
		t.Fatal("echo should not be an error")
	}
	if res != "echo: hello mcp" {
		t.Fatalf("echo result = %q", res)
	}

	// tool-level error surfaces via isError, not err
	res, isError, err = hub.Call(context.Background(), "always_fails", `{"reason":"boom"}`, CallMeta{})
	if err != nil {
		t.Fatalf("call always_fails: %v", err)
	}
	if !isError {
		t.Fatal("always_fails must report isError")
	}
	if !strings.Contains(res, "boom") {
		t.Fatalf("always_fails result = %q", res)
	}

	// unknown tool
	if _, _, err := hub.Call(context.Background(), "nope", `{}`, CallMeta{}); err == nil {
		t.Fatal("unknown tool must error")
	}

	// Arguments are model output on the way to an external server: only a
	// JSON object (or nothing) is accepted.
	if _, _, err := hub.Call(context.Background(), "echo", `[1,2]`, CallMeta{}); err == nil {
		t.Fatal("non-object arguments must be rejected")
	}
	if _, _, err := hub.Call(context.Background(), "echo", `"plain string"`, CallMeta{}); err == nil {
		t.Fatal("non-object arguments must be rejected")
	}
	// Empty arguments are accepted by the hub; the server's schema then
	// decides (here: text is required, so the tool reports its own error).
	res, isError, err = hub.Call(context.Background(), "echo", "", CallMeta{})
	if err != nil || !isError || !strings.Contains(res, "text") {
		t.Fatalf("empty args: res=%q isError=%v err=%v, want a server-side validation error", res, isError, err)
	}
}

func TestHubReload(t *testing.T) {
	st := testStore(t, testConfig())
	hub := New(st)
	hub.Connect(context.Background())
	defer hub.Close()
	if n := len(hub.Tools()); n != 2 {
		t.Fatalf("initial tools = %d, want 2", n)
	}

	// Add a second server through the config store, then Reload picks it up.
	cur := st.Get()
	added := append(append([]config.MCPServerConfig{}, cur.MCPServers...),
		config.MCPServerConfig{Name: "b", Transport: "stdio", Command: testServerBinary, DefaultEnabled: true})
	if _, err := st.Update(context.Background(), config.Patch{MCPServers: &added}); err != nil {
		t.Fatalf("update: %v", err)
	}
	hub.Reload(context.Background())
	if n := len(hub.Tools()); n != 4 {
		t.Fatalf("after add: tools = %d, want 4", n)
	}
	if res, _, err := hub.Call(context.Background(), "echo_2", `{"text":"hi"}`, CallMeta{}); err != nil || res != "echo: hi" {
		t.Fatalf("echo_2 after reload: res=%q err=%v", res, err)
	}

	// Nothing changed between reloads: Reload reports no change.
	if hub.Reload(context.Background()) {
		t.Fatal("Reload without config change reported a catalog change")
	}

	// Remove the second server again.
	if _, err := st.Update(context.Background(), config.Patch{MCPServers: &cur.MCPServers}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !hub.Reload(context.Background()) {
		t.Fatal("Reload after server removal reported no catalog change")
	}
	if n := len(hub.Tools()); n != 2 {
		t.Fatalf("after remove: tools = %d, want 2", n)
	}

	// Change a remaining server's config (default_enabled) → reconnect, still 2 tools.
	changed := []config.MCPServerConfig{
		{Name: "test", Transport: "stdio", Command: testServerBinary, DefaultEnabled: false},
	}
	if _, err := st.Update(context.Background(), config.Patch{MCPServers: &changed}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !hub.Reload(context.Background()) {
		t.Fatal("Reload after server config change reported no catalog change")
	}
	tools := hub.Tools()
	if len(tools) != 2 {
		t.Fatalf("after change: tools = %d, want 2", len(tools))
	}
	if tools[0].DefaultEnabled {
		t.Fatal("default_enabled change not picked up by reconnect")
	}
}

func TestHubCollisionSuffix(t *testing.T) {
	cfg := config.Config{
		MCPServers: []config.MCPServerConfig{
			{Name: "a", Transport: "stdio", Command: testServerBinary, DefaultEnabled: true},
			{Name: "b", Transport: "stdio", Command: testServerBinary, DefaultEnabled: false},
		},
	}
	hub := New(testStore(t, cfg))
	hub.Connect(context.Background())
	defer hub.Close()

	names := map[string]bool{}
	for _, e := range hub.Tools() {
		names[e.Display] = true
	}
	// first server keeps bare names; second gets _2 suffixes
	for _, want := range []string{"echo", "always_fails", "echo_2", "always_fails_2"} {
		if !names[want] {
			t.Fatalf("missing suffixed tool %q in %v", want, names)
		}
	}
	res, _, err := hub.Call(context.Background(), "echo_2", `{"text":"suffixed"}`, CallMeta{})
	if err != nil || res != "echo: suffixed" {
		t.Fatalf("suffixed call: res=%q err=%v", res, err)
	}
}

// TestCollisionRename reserves renamed names too: a rename must never
// collide with an existing entry or a later one (the old count-based
// scheme produced duplicate display names in this scenario).
func TestCollisionRename(t *testing.T) {
	hub := &Hub{entries: []Entry{
		{Display: "echo", Server: "a"},
		{Display: "echo", Server: "b"},
		{Display: "echo_2", Server: "b"},
		{Display: "echo", Server: "c"},
	}}
	hub.applyCollisionSuffixesLocked()

	seen := map[string]bool{}
	var got []string
	for _, e := range hub.entries {
		got = append(got, e.Display)
		if seen[e.Display] {
			t.Fatalf("duplicate display name %q in %v", e.Display, got)
		}
		seen[e.Display] = true
	}
	want := []string{"echo", "echo_2", "echo_2_2", "echo_3"}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("entries = %v, want %v", got, want)
		}
	}
}

func TestResultText(t *testing.T) {
	text := func(s string) mcp.Content { return &mcp.TextContent{Text: s} }

	cases := []struct {
		name string
		res  *mcp.CallToolResult
		want string
	}{
		{"texts joined", &mcp.CallToolResult{Content: []mcp.Content{text("a"), text("b")}}, "a\nb"},
		{"non-text noted", &mcp.CallToolResult{Content: []mcp.Content{text("a"), &mcp.ImageContent{}}}, "a\n[*mcp.ImageContent content omitted]"},
		{"structured fallback", &mcp.CallToolResult{StructuredContent: map[string]any{"k": "v"}}, `{"k":"v"}`},
		{"empty", &mcp.CallToolResult{}, ""},
	}
	for _, tc := range cases {
		got, err := resultText(tc.res)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s: resultText = %q, want %q", tc.name, got, tc.want)
		}
	}
}
