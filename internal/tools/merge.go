package tools

import (
	"context"
	"fmt"
	"log/slog"

	"chattoneko/internal/config"
	"chattoneko/internal/mcphub"
)

// source is a tool catalog: the contract the engine and the API consume.
// Both *registry (integrated tools) and *mcphub.hub (MCP tools) implement it.
type source interface {
	Tools() []mcphub.Entry
	Call(ctx context.Context, display, argsJSON string, meta mcphub.CallMeta) (string, bool, error)
}

// Merged is the union of several tool sources presented as one catalog, so
// the engine doesn't care where a tool comes from. The union is computed
// LAZILY on every Tools()/Call() from the live sources, so sources whose
// tool list changes at runtime (the MCP hub reconnecting after a config
// update) are picked up without rebuilding the catalog. On display-name
// collisions the FIRST source passed wins (pass integrated tools first).
//
// cfg supplies the global per-tool defaults from the settings UI
// (tool_defaults), read live on every Tools() call: an entry listed there
// gets its DefaultEnabled replaced, so both the engine's per-chat effective
// tool set and the API's tool listing see the configured default. A tool
// absent from the map keeps its source's own default (integrated tools:
// hardcoded; MCP tools: their server's default_enabled). The same goes for
// the user-facing title: cfg's tool_titles map (tool name → label, edited in
// the settings UI for MCP tools) replaces an entry's Title when it holds a
// non-empty value, otherwise the source's own stays (integrated tools:
// hardcoded; MCP tools: the title their server declared). cfg may be nil.
type Merged struct {
	cfg     *config.Store
	sources []source
}

// Merge combines tool sources into one live catalog with the configured
// global tool defaults layered over the sources' own defaults.
func Merge(cfg *config.Store, sources ...source) *Merged {
	return &Merged{cfg: cfg, sources: sources}
}

// dedup merges the sources' tool lists, first source wins on display-name
// collisions (the later entry is dropped, keeping Tools and Call consistent),
// and applies the configured per-tool defaults and titles.
func (m *Merged) dedup() []mcphub.Entry {
	var over map[string]bool
	var titles map[string]string
	if m.cfg != nil {
		cfg := m.cfg.Get()
		over = cfg.ToolDefaults
		titles = cfg.ToolTitles
	}
	owners := map[string]bool{}
	var out []mcphub.Entry
	for _, src := range m.sources {
		for _, e := range src.Tools() {
			if owners[e.Display] {
				slog.Warn("tool name collision; dropping later entry",
					"name", e.Display, "server", e.Server)
				continue
			}
			owners[e.Display] = true
			if v, ok := over[e.Display]; ok {
				e.DefaultEnabled = v
			}
			// Only a non-empty configured title wins: clearing the settings
			// input must hand the label back to the source, not blank it.
			if v := titles[e.Display]; v != "" {
				e.Title = v
			}
			out = append(out, e)
		}
	}
	return out
}

// Tools returns the merged, deduplicated catalog.
func (m *Merged) Tools() []mcphub.Entry {
	return m.dedup()
}

// Call routes the call to the first source that owns the tool name.
func (m *Merged) Call(ctx context.Context, display, argsJSON string, meta mcphub.CallMeta) (string, bool, error) {
	for _, src := range m.sources {
		for _, e := range src.Tools() {
			if e.Display == display {
				return src.Call(ctx, display, argsJSON, meta)
			}
		}
	}
	return "", false, fmt.Errorf("unknown tool %q", display)
}
