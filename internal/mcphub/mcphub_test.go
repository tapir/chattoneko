package mcphub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"chattoneko/internal/config"
	"chattoneko/internal/db"
)

// testMCPServer runs an in-process MCP server over streamable HTTP and
// returns its endpoint. It exposes two tools: "echo" (succeeds) and
// "always_fails" (returns an MCP tool error).
func testMCPServer(t *testing.T) string {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "testmcp", Version: "1.0.0"}, nil)

	type echoArgs struct {
		Text string `json:"text" jsonschema:"the text to echo back"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "echo",
		Description: "echo the given text back",
		Title:       "Echoing…",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args echoArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + args.Text}},
		}, nil, nil
	})

	type failArgs struct {
		Reason string `json:"reason" jsonschema:"why it fails"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "always_fails",
		Description: "always returns a tool error",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args failArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "tool error: " + args.Reason}},
			IsError: true,
		}, nil, nil
	})

	hs := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(hs.Close)
	return hs.URL
}

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

func testConfig(url string) config.Config {
	return config.Config{
		MCPServers: []config.MCPServerConfig{serverCfg("test", url, true)},
	}
}

func serverCfg(name, url string, enabled bool) config.MCPServerConfig {
	return config.MCPServerConfig{Name: name, Transport: "http", URL: url, DefaultEnabled: enabled}
}

func TestHubListAndCall(t *testing.T) {
	hub := New(testStore(t, testConfig(testMCPServer(t))))
	hub.Reload(context.Background())
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
	// The server's own display title reaches the catalog; a tool that declares
	// none (always_fails) keeps Title empty and the UI shows its raw name.
	if names["echo"].Title != "Echoing…" || names["always_fails"].Title != "" {
		t.Fatalf("titles wrong: echo=%q always_fails=%q", names["echo"].Title, names["always_fails"].Title)
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
	url := testMCPServer(t)
	st := testStore(t, testConfig(url))
	hub := New(st)
	hub.Reload(context.Background())
	defer hub.Close()
	if n := len(hub.Tools()); n != 2 {
		t.Fatalf("initial tools = %d, want 2", n)
	}

	// Add a second server through the config store, then Reload picks it up.
	cur := st.Get()
	added := append(append([]config.MCPServerConfig{}, cur.MCPServers...), serverCfg("b", url, true))
	if _, err := st.Update(context.Background(), config.Patch{MCPServers: &added}); err != nil {
		t.Fatalf("update: %v", err)
	}
	hub.Reload(context.Background())
	// Server b points at the same endpoint, so both of its tools collide with
	// the first server's and are dropped (first server in config order wins).
	if n := len(hub.Tools()); n != 2 {
		t.Fatalf("after add: tools = %d, want 2 (b's duplicates dropped)", n)
	}
	if res, _, err := hub.Call(context.Background(), "echo", `{"text":"hi"}`, CallMeta{}); err != nil || res != "echo: hi" {
		t.Fatalf("echo after reload: res=%q err=%v", res, err)
	}
	if _, _, err := hub.Call(context.Background(), "echo_2", `{"text":"hi"}`, CallMeta{}); err == nil {
		t.Fatal("a collided tool must not be reachable under a suffixed name")
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
	changed := []config.MCPServerConfig{serverCfg("test", url, false)}
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

func TestHubCollisionFirstWins(t *testing.T) {
	url := testMCPServer(t)
	cfg := config.Config{
		MCPServers: []config.MCPServerConfig{serverCfg("a", url, true), serverCfg("b", url, false)},
	}
	hub := New(testStore(t, cfg))
	hub.Reload(context.Background())
	defer hub.Close()

	// Both servers expose the same tools: the FIRST server in config order
	// keeps the name, the later duplicate is dropped (never renamed), so the
	// catalog holds unique display names.
	tools := hub.Tools()
	if len(tools) != 2 {
		t.Fatalf("tools = %d, want 2: %v", len(tools), tools)
	}
	for _, e := range tools {
		if e.Server != "a" {
			t.Fatalf("tool %q owned by server %q, want the first server \"a\"", e.Display, e.Server)
		}
	}
	res, _, err := hub.Call(context.Background(), "echo", `{"text":"first"}`, CallMeta{})
	if err != nil || res != "echo: first" {
		t.Fatalf("call: res=%q err=%v", res, err)
	}
	if _, _, err := hub.Call(context.Background(), "echo_2", `{"text":"x"}`, CallMeta{}); err == nil {
		t.Fatal("suffixed name must not exist")
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

// Probe is the settings UI's per-card "fetch tools" path: it dials ONE server
// config outside any hub, so it must report that config's tools (labelled with
// the posted name) and must not need a hub at all.
func TestProbe(t *testing.T) {
	entries, err := Probe(t.Context(), serverCfg("probe", testMCPServer(t), true))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 tools, got %d: %+v", len(entries), entries)
	}
	for _, e := range entries {
		if e.Server != "probe" || !e.DefaultEnabled || len(e.Schema) == 0 {
			t.Fatalf("entry not built from the probed config: %+v", e)
		}
	}
	// A dead endpoint must be an error (the handler turns it into a toast),
	// never a silent empty list.
	if _, err := Probe(t.Context(), serverCfg("dead", "http://127.0.0.1:1/mcp", true)); err == nil {
		t.Fatal("probe of an unreachable server should fail")
	}
}
