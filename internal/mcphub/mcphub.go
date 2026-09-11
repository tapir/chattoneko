// Package mcphub manages the MCP client connections declared in config.
// The server list and the per-call timeout come from the live config store:
// Reload reconciles connections after a config change (connect new servers,
// close removed ones, reconnect changed ones) without a restart.
package mcphub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"chattoneko/internal/config"
)

// CallMeta carries the conversation coordinates of one tool call: which
// chat and which assistant message (the one holding the call) it belongs
// to. Integrated tools use it to attach artifacts (e.g. generated files) to
// the right message; MCP tools ignore it. It lives here (not in
// internal/tools) because this package is the shared tool vocabulary —
// internal/tools already imports it for Entry.
type CallMeta struct {
	ChatID    string
	MessageID string // assistant message owning the tool call
}

// BuiltinServer is the Entry.Server value integrated tools (internal/tools)
// carry in place of a config server name. The UI shows it as the tool's
// origin, and the engine reads it to tell integrated tools from MCP ones —
// only MCP calls are charged against the per-response tool-call budget. It
// lives here for the same reason CallMeta does: shared tool vocabulary.
const BuiltinServer = "builtin"

// Entry is one tool in the aggregated catalog.
type Entry struct {
	Display        string          `json:"name"`            // LLM-facing name (unique); JSON key "name" per API contract
	Description    string          `json:"description"`     // tool description
	Server         string          `json:"server"`          // config server name
	Schema         json.RawMessage `json:"schema"`          // JSON schema for arguments
	DefaultEnabled bool            `json:"default_enabled"` // config default toggle
	// Title is the USER-facing label the UI shows instead of the raw name
	// ("Coding…" for code). Empty means "no title" — the UI falls back
	// to Display. Never sent to the model, which only ever sees Display.
	Title string `json:"title"`
}

// connectTimeout bounds dialing + tool listing for one MCP server so a dead
// server can't stall startup.
const connectTimeout = 30 * time.Second

// serverState is one connected MCP server.
type serverState struct {
	cfg     config.MCPServerConfig
	session *mcp.ClientSession
	entries []Entry
}

// hub owns one ClientSession per connected MCP server.
type hub struct {
	store *config.Store

	mu       sync.RWMutex
	servers  map[string]*serverState       // by config server name
	entries  []Entry                       // merged catalog, first server wins name collisions
	sessions map[string]*mcp.ClientSession // by display name, for Call

	reloadMu sync.Mutex // serializes Connect/Reload reconciliation
}

// New creates a hub reading its server list from the config store. Call
// Reload to dial the servers.
func New(store *config.Store) *hub {
	return &hub{store: store, servers: map[string]*serverState{}}
}

// Reload re-reads the MCP server list from the config store and reconciles:
// new servers are connected CONCURRENTLY (a dead server must not serialize
// its network timeout into boot or reload time), removed ones closed, changed
// ones (any field) reconnected. Per-server failures are logged and skipped
// (their tools stay absent). Safe to call concurrently — reconciliation is
// serialized. Returns true when the reconciliation changed the tool catalog
// (servers were added, removed, or reconnected), so callers can notify
// clients that /api/config is stale.
func (h *hub) Reload(ctx context.Context) bool {
	h.reloadMu.Lock()
	defer h.reloadMu.Unlock()
	return h.reconcileLocked(ctx, h.store.Get().MCPServers)
}

// reconcileLocked diffs the desired server list against the current
// connections and applies the minimum set of connect/close operations.
// Callers must hold reloadMu. Returns true when the tool catalog changed
// (something was closed or a new connection succeeded).
func (h *hub) reconcileLocked(ctx context.Context, desired []config.MCPServerConfig) bool {
	// Decide what to close: servers removed from config, or whose config
	// changed (they will be reconnected below). reloadMu already serializes
	// reconciliation and st.cfg is immutable once inserted, so the live map
	// can be read under RLock — no snapshot copy.
	var toClose []string
	h.mu.RLock()
	for name, st := range h.servers {
		found := false
		for _, d := range desired {
			if d.Name == name {
				found = true
				if !config.MCPServerEqual(st.cfg, d) {
					toClose = append(toClose, name)
				}
				break
			}
		}
		if !found {
			toClose = append(toClose, name)
		}
	}
	h.mu.RUnlock()
	if len(toClose) > 0 {
		// Detach under the lock, close OUTSIDE it: session teardown does
		// HTTP I/O and must not block every Tools()/Call() reader while it
		// runs.
		var closing []*serverState
		h.mu.Lock()
		for _, name := range toClose {
			if st, ok := h.servers[name]; ok {
				closing = append(closing, st)
				delete(h.servers, name)
			}
		}
		h.mu.Unlock()
		for _, st := range closing {
			if err := st.session.Close(); err != nil {
				slog.Warn("mcp session close failed", "name", st.cfg.Name, "error", err)
			}
		}
	}

	// Decide what to connect: desired servers not currently connected.
	var toConnect []config.MCPServerConfig
	h.mu.RLock()
	for _, d := range desired {
		if _, ok := h.servers[d.Name]; !ok {
			toConnect = append(toConnect, d)
		}
	}
	h.mu.RUnlock()

	type result struct {
		name  string
		state *serverState
	}
	results := make([]result, len(toConnect))
	var wg sync.WaitGroup
	for i, sc := range toConnect {
		wg.Add(1)
		go func(i int, sc config.MCPServerConfig) {
			defer wg.Done()
			session, entries := h.connectOne(ctx, sc)
			if session != nil {
				results[i] = result{name: sc.Name, state: &serverState{cfg: sc, session: session, entries: entries}}
			}
		}(i, sc)
	}
	wg.Wait()

	h.mu.Lock()
	defer h.mu.Unlock()
	connected := 0
	for _, r := range results {
		if r.state != nil {
			h.servers[r.name] = r.state
			connected++
		}
	}
	// Rebuild the merged catalog in config order.
	order := make([]string, 0, len(desired))
	for _, d := range desired {
		if _, ok := h.servers[d.Name]; ok {
			order = append(order, d.Name)
		}
	}
	h.rebuildEntriesLocked(order)
	return len(toClose) > 0 || connected > 0
}

// rebuildEntriesLocked merges per-server entries in config order and indexes
// the session that owns each display name. On a name collision the FIRST
// server in config order wins and the later entry is dropped — the same
// policy the merged catalog applies across sources (tools.Merge).
// Callers must hold h.mu for writing.
func (h *hub) rebuildEntriesLocked(order []string) {
	h.entries = nil
	h.sessions = make(map[string]*mcp.ClientSession)
	for _, name := range order {
		st := h.servers[name]
		if st == nil {
			continue
		}
		for _, e := range st.entries {
			if _, dup := h.sessions[e.Display]; dup {
				slog.Warn("mcp tool name collision; dropping later entry",
					"name", e.Display, "server", e.Server)
				continue
			}
			h.entries = append(h.entries, e)
			h.sessions[e.Display] = st.session
		}
	}
}

// dial connects a fresh client session to one server config. The caller owns
// the returned session.
func dial(ctx context.Context, sc config.MCPServerConfig) (*mcp.ClientSession, error) {
	transport := &mcp.StreamableClientTransport{Endpoint: sc.URL}
	if len(sc.Headers) > 0 {
		transport.HTTPClient = &http.Client{Transport: headerTransport{base: http.DefaultTransport, headers: sc.Headers}}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "chattoneko", Version: "1.0.0"}, nil)
	return client.Connect(ctx, transport, nil)
}

// Probe dials ONE server config outside any hub, lists its tools and closes
// the session again. This is the settings UI's per-card "fetch tools" path:
// it must work for a server that is brand new or has unsaved url/header
// edits, so it never touches the hub's live connections or the catalog.
func Probe(ctx context.Context, sc config.MCPServerConfig) ([]Entry, error) {
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	session, err := dial(cctx, sc)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()
	return listTools(cctx, session, sc)
}

// connectOne dials one server and lists its tools; on failure it logs a
// warning and returns nil session (the server is skipped).
func (h *hub) connectOne(ctx context.Context, sc config.MCPServerConfig) (*mcp.ClientSession, []Entry) {
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	session, err := dial(cctx, sc)
	if err != nil {
		slog.Warn("mcp server connect failed; its tools will be unavailable",
			"name", sc.Name, "error", err)
		return nil, nil
	}
	entries, err := listTools(cctx, session, sc)
	if err != nil {
		slog.Warn("mcp server tool listing failed", "name", sc.Name, "error", err)
		_ = session.Close()
		return nil, nil
	}
	slog.Info("mcp server connected", "name", sc.Name, "tools", len(entries))
	return session, entries
}

// maxListToolsPages caps one server's tool-listing pagination: a broken or
// hostile server returning a cycling NextCursor must not be able to spin the
// listing forever (the connect timeout bounds it too, but an explicit cap
// keeps the outcome sane).
const maxListToolsPages = 100

func listTools(ctx context.Context, session *mcp.ClientSession, sc config.MCPServerConfig) ([]Entry, error) {
	var out []Entry
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxListToolsPages {
			slog.Warn("mcp server tool listing truncated: too many pages", "name", sc.Name, "pages", page)
			break
		}
		res, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, t := range res.Tools {
			schema, err := json.Marshal(t.InputSchema)
			if err != nil || string(schema) == "null" || len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			out = append(out, Entry{
				Display:        t.Name,
				Description:    t.Description,
				Server:         sc.Name,
				Schema:         schema,
				DefaultEnabled: sc.DefaultEnabled,
				Title:          mcpTitle(t),
			})
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return out, nil
}

// mcpTitle is the display title the MCP server itself declared, using the
// spec's own precedence (title, then annotations.title). Empty when the
// server sent neither — the configured tool_titles override (see tools.Merge)
// and, failing that, the raw name is what the UI shows.
func mcpTitle(t *mcp.Tool) string {
	if t.Title != "" {
		return t.Title
	}
	if t.Annotations != nil {
		return t.Annotations.Title
	}
	return ""
}

// Tools returns the aggregated catalog.
func (h *hub) Tools() []Entry {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]Entry, len(h.entries))
	copy(out, h.entries)
	return out
}

// Call invokes a tool by display name. argsJSON must be a JSON object (or "").
// Returns the result rendered as text (see resultText) and the isError flag
// from the tool. The CallMeta is part of the shared catalog contract; MCP
// tools don't use it.
func (h *hub) Call(ctx context.Context, display, argsJSON string, _ CallMeta) (string, bool, error) {
	h.mu.RLock()
	session, ok := h.sessions[display]
	h.mu.RUnlock()
	if !ok {
		return "", false, fmt.Errorf("unknown tool %q", display)
	}

	// Tool arguments are model output on the way to an external server:
	// accept only what the MCP spec defines — a JSON object (or nothing).
	var args map[string]any
	if s := strings.TrimSpace(argsJSON); s != "" {
		if err := json.Unmarshal([]byte(s), &args); err != nil {
			return "", false, fmt.Errorf("tool %q: invalid arguments JSON (must be an object): %w", display, err)
		}
	}
	// Bound every tool call: a hung MCP server must not block the turn loop
	// indefinitely. The timeout is read live from config.
	timeout := time.Duration(h.store.Get().Limits.MCPCallTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := session.CallTool(callCtx, &mcp.CallToolParams{Name: display, Arguments: args})
	if err != nil {
		return "", false, fmt.Errorf("tool %q: %w", display, err)
	}
	text, err := resultText(res)
	if err != nil {
		return "", false, fmt.Errorf("tool %q: %w", display, err)
	}
	return text, res.IsError, nil
}

// resultText renders a tool result as text: all text content blocks joined
// by newlines. Non-text blocks (images, audio, resources) are replaced with
// a placeholder so they aren't silently dropped. If the result carries no text
// at all, structured content is rendered as JSON instead, so servers that
// only return structured results don't surface as empty strings.
func resultText(res *mcp.CallToolResult) (string, error) {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
			continue
		}
		parts = append(parts, fmt.Sprintf("[%T content omitted]", c))
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n"), nil
	}
	if res.StructuredContent != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return "", fmt.Errorf("marshal structured content: %w", err)
		}
		return string(b), nil
	}
	return "", nil
}

// headerTransport adds the config-declared HTTP headers (e.g. API keys) to
// every request the streamable MCP client sends.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	return t.base.RoundTrip(req)
}

// Close closes all sessions OUTSIDE the lock: teardown I/O must not block
// catalog readers.
func (h *hub) Close() {
	h.mu.Lock()
	closing := h.servers
	h.servers = map[string]*serverState{}
	h.entries = nil
	h.sessions = nil
	h.mu.Unlock()
	for name, st := range closing {
		if err := st.session.Close(); err != nil {
			slog.Warn("mcp session close failed", "name", name, "error", err)
		}
	}
}
