package tools

import (
	"context"
	"fmt"
	"log/slog"

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
type Merged struct {
	sources []Source
}

// Merge combines tool sources into one live catalog.
func Merge(sources ...Source) *Merged {
	return &Merged{sources: sources}
}

// dedup merges the sources' tool lists, first source wins on display-name
// collisions (the later entry is dropped, keeping Tools and Call consistent).
func (m *Merged) dedup() []mcphub.Entry {
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
