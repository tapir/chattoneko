package tools

import (
	"chattoneko/internal/config"
	"chattoneko/internal/mcphub"
)

// Defaults layers the global per-tool defaults from the config (settings UI)
// over a catalog: entry DefaultEnabled is replaced by config tool_defaults
// when the tool is listed there, so both the engine's per-chat effective tool
// set and the API's tool listing see the same configured default. A tool
// absent from the map keeps its source's own default (integrated tools:
// hardcoded; MCP tools: their server's default_enabled). Call is promoted
// from the embedded source.
type Defaults struct {
	Source
	cfg *config.Store
}

// WithDefaults wraps src with the configured global tool defaults, read live
// from cfg on every Tools() call so a settings save applies immediately.
func WithDefaults(src Source, cfg *config.Store) *Defaults {
	return &Defaults{Source: src, cfg: cfg}
}

// Tools returns the wrapped catalog with the configured defaults applied.
// Sources build a fresh slice per call, so overriding in place is safe.
func (d *Defaults) Tools() []mcphub.Entry {
	out := d.Source.Tools()
	over := d.cfg.Get().ToolDefaults
	for i, e := range out {
		if v, ok := over[e.Display]; ok {
			out[i].DefaultEnabled = v
		}
	}
	return out
}
