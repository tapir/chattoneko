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
	"os/exec"
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

// Entry is one tool in the aggregated catalog.
type Entry struct {
	Display        string          `json:"name"`            // LLM-facing name (unique); JSON key "name" per API contract
	Description    string          `json:"description"`     // tool description
	Server         string          `json:"server"`          // config server name
	Schema         json.RawMessage `json:"schema"`          // JSON schema for arguments
	DefaultEnabled bool            `json:"default_enabled"` // config default toggle
	realName       string          // name on the MCP server
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

// route is what Call needs to dispatch one display name.
type route struct {
	session  *mcp.ClientSession
	realName string
}

// Hub owns one ClientSession per connected MCP server.
type Hub struct {
	store *config.Store

	mu      sync.RWMutex
	servers map[string]*serverState // by config server name
	entries []Entry                 // merged catalog with collision suffixes
	routes  map[string]route        // by display name, for Call

	reloadMu sync.Mutex // serializes Connect/Reload reconciliation
}

// New creates a hub reading its server list from the config store. Call
// Connect to dial the servers.
func New(store *config.Store) *Hub {
	return &Hub{store: store, servers: map[string]*serverState{}}
}

// Connect dials every configured MCP server CONCURRENTLY (a dead server must
// not serialize its network timeout into boot time) and builds the tool
// catalog in config order. Per-server failures are logged and skipped (their
// tools stay absent).
func (h *Hub) Connect(ctx context.Context) {
	h.reloadMu.Lock()
	defer h.reloadMu.Unlock()
	h.reconcileLocked(ctx, h.store.Get().MCPServers)
}

// Reload re-reads the MCP server list from the config store and reconciles:
// new servers are connected, removed ones closed, changed ones (any field)
// reconnected. Safe to call concurrently — reconciliation is serialized.
// Returns true when the reconciliation changed the tool catalog (servers
// were added, removed, or reconnected), so callers can notify clients that
// /api/config is stale.
func (h *Hub) Reload(ctx context.Context) bool {
	h.reloadMu.Lock()
	defer h.reloadMu.Unlock()
	return h.reconcileLocked(ctx, h.store.Get().MCPServers)
}

// reconcileLocked diffs the desired server list against the current
// connections and applies the minimum set of connect/close operations.
// Callers must hold reloadMu. Returns true when the tool catalog changed
// (something was closed or a new connection succeeded).
func (h *Hub) reconcileLocked(ctx context.Context, desired []config.MCPServerConfig) bool {
	// Snapshot current servers outside the lock.
	h.mu.Lock()
	current := make(map[string]*serverState, len(h.servers))
	for name, st := range h.servers {
		current[name] = st
	}
	h.mu.Unlock()

	// Decide what to close: servers removed from config, or whose config
	// changed (they will be reconnected below).
	var toClose []string
	for name, st := range current {
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
	if len(toClose) > 0 {
		// Detach under the lock, close OUTSIDE it: session teardown can do
		// I/O (stdio process kill, HTTP teardown) and must not block every
		// Tools()/Call() reader while it runs.
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

// rebuildEntriesLocked merges per-server entries in config order and makes
// display names unique. Callers must hold h.mu for writing.
func (h *Hub) rebuildEntriesLocked(order []string) {
	h.entries = nil
	for _, name := range order {
		if st := h.servers[name]; st != nil {
			h.entries = append(h.entries, st.entries...)
		}
	}
	h.applyCollisionSuffixesLocked()
	h.routes = make(map[string]route, len(h.entries))
	for i := range h.entries {
		e := &h.entries[i]
		if st := h.servers[e.Server]; st != nil {
			h.routes[e.Display] = route{session: st.session, realName: e.realName}
		}
	}
}

// connectOne dials one server and lists its tools; on failure it logs a
// warning and returns nil session (the server is skipped).
func (h *Hub) connectOne(ctx context.Context, sc config.MCPServerConfig) (*mcp.ClientSession, []Entry) {
	var transport mcp.Transport
	switch sc.Transport {
	case "stdio":
		transport = &mcp.CommandTransport{Command: exec.Command(sc.Command, sc.Args...)}
	case "http":
		st := &mcp.StreamableClientTransport{Endpoint: sc.URL}
		if len(sc.Headers) > 0 {
			st.HTTPClient = &http.Client{Transport: headerTransport{base: http.DefaultTransport, headers: sc.Headers}}
		}
		transport = st
	default:
		slog.Warn("mcp server with unknown transport", "name", sc.Name, "transport", sc.Transport)
		return nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "chattoneko", Version: "1.0.0"}, nil)
	session, err := client.Connect(cctx, transport, nil)
	if err != nil {
		slog.Warn("mcp server connect failed; its tools will be unavailable",
			"name", sc.Name, "error", err)
		return nil, nil
	}
	entries, err := h.listTools(cctx, session, sc)
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

func (h *Hub) listTools(ctx context.Context, session *mcp.ClientSession, sc config.MCPServerConfig) ([]Entry, error) {
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
				realName:       t.Name,
			})
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return out, nil
}

// applyCollisionSuffixesLocked makes display names unique across servers
// (the first entry keeps the bare name; later collisions get _2, _3, ...).
// Renamed names are reserved too, so a rename can never collide with an
// existing or later entry. Callers must hold h.mu.
func (h *Hub) applyCollisionSuffixesLocked() {
	seen := make(map[string]bool, len(h.entries))
	for i := range h.entries {
		e := &h.entries[i]
		name := e.Display
		for n := 2; seen[name]; n++ {
			name = fmt.Sprintf("%s_%d", e.Display, n)
		}
		if name != e.Display {
			slog.Warn("mcp tool name collision; renamed", "from", e.Display, "to", name, "server", e.Server)
			e.Display = name
		}
		seen[name] = true
	}
}

// Tools returns the aggregated catalog.
func (h *Hub) Tools() []Entry {
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
func (h *Hub) Call(ctx context.Context, display, argsJSON string, _ CallMeta) (string, bool, error) {
	h.mu.RLock()
	rt, ok := h.routes[display]
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
	res, err := rt.session.CallTool(callCtx, &mcp.CallToolParams{Name: rt.realName, Arguments: args})
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

// Close closes all sessions (terminates stdio child processes). Sessions
// are closed OUTSIDE the lock: teardown I/O must not block catalog readers.
func (h *Hub) Close() {
	h.mu.Lock()
	closing := h.servers
	h.servers = map[string]*serverState{}
	h.entries = nil
	h.routes = nil
	h.mu.Unlock()
	for name, st := range closing {
		if err := st.session.Close(); err != nil {
			slog.Warn("mcp session close failed", "name", name, "error", err)
		}
	}
}
