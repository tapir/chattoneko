package tools

import (
	"context"
	"fmt"
	"log/slog"

	"chattoneko/internal/config"
	"chattoneko/internal/mcphub"
)

// Source is a tool catalog: the contract the engine and the API consume.
// Both *Registry (integrated tools) and *mcphub.Hub (MCP tools) implement it.
type Source interface {
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
// hardcoded; MCP tools: their server's default_enabled). cfg may be nil.
type Merged struct {
	cfg     *config.Store
	sources []Source
}

// Merge combines tool sources into one live catalog with the configured
// global tool defaults layered over the sources' own defaults.
func Merge(cfg *config.Store, sources ...Source) *Merged {
	return &Merged{cfg: cfg, sources: sources}
}

// dedup merges the sources' tool lists, first source wins on display-name
// collisions (the later entry is dropped, keeping Tools and Call consistent),
// and applies the configured per-tool defaults.
func (m *Merged) dedup() []mcphub.Entry {
	var over map[string]bool
	if m.cfg != nil {
		over = m.cfg.Get().ToolDefaults
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
