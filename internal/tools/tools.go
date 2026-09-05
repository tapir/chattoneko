// Package tools provides the integrated (built-in) tool catalog: tools
// implemented in-process and declared entirely in code. Integrated tools
// implement the same engine.ToolCatalog interface as the MCP hub, so the
// engine's turn loop, per-chat toggles, and the API's tool listing treat
// them identically to MCP tools.
//
// Adding a new integrated tool:
//  1. Create a file for it here (e.g. timelocation.go) with a `var MyTool = Tool{...}`
//     holding all LLM-facing text (name, description, schema) hardcoded.
//  2. Add it to the list in Builtin() (catalog.go).
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"chattoneko/internal/mcphub"
)

// serverLabel is the Entry.Server value for integrated tools. The UI shows
// it as the tool's origin; it has no functional effect.
const serverLabel = "builtin"

// callTimeout bounds one integrated tool call: integrated tools are local,
// but a future handler doing I/O must not block the turn loop indefinitely.
const callTimeout = 30 * time.Second

// Tool is one integrated tool definition. All LLM-facing text is hardcoded
// here in code — edit the tool's own file to change it. DefaultEnabled
// controls whether the tool starts enabled for chats that have not toggled
// it explicitly (per-chat overrides live in the chat's persisted tools map).
type Tool struct {
	Name           string          // LLM-facing name (must be unique across the whole catalog)
	Description    string          // LLM-facing description
	Schema         json.RawMessage // JSON schema for the arguments
	DefaultEnabled bool            // enabled by default (configurable here in code)
	Handler        Handler
}

// Handler executes one tool call. argsJSON is the raw arguments JSON the
// model produced ("" when the model sent none); handlers that take arguments
// should json.Unmarshal and validate them themselves. meta carries the
// chat/message coordinates of the call for handlers that persist artifacts
// (e.g. create_text_file attaching its file to the assistant message).
// The returned string is what the model sees as the tool result.
type Handler func(ctx context.Context, argsJSON string, meta mcphub.CallMeta) (string, error)

// Registry is a static catalog of integrated tools. It implements
// engine.ToolCatalog.
type Registry struct {
	tools  []Tool
	byName map[string]int // name → index into tools
}

// New builds a Registry from the given tool definitions. Duplicate names are
// a programming error and panic at startup so they are caught immediately.
func New(ts ...Tool) *Registry {
	r := &Registry{byName: map[string]int{}}
	for i := range ts {
		t := ts[i]
		if t.Name == "" || t.Handler == nil {
			panic(fmt.Sprintf("tools: integrated tool %d has no name or no handler", i))
		}
		if _, dup := r.byName[t.Name]; dup {
			panic("tools: duplicate integrated tool name " + t.Name)
		}
		if len(t.Schema) == 0 {
			t.Schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		r.byName[t.Name] = len(r.tools)
		r.tools = append(r.tools, t)
	}
	return r
}

// Tools returns the catalog entries for all integrated tools, in declaration
// order.
func (r *Registry) Tools() []mcphub.Entry {
	out := make([]mcphub.Entry, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, mcphub.Entry{
			Display:        t.Name,
			Description:    t.Description,
			Server:         serverLabel,
			Schema:         t.Schema,
			DefaultEnabled: t.DefaultEnabled,
		})
	}
	return out
}

// Call invokes the named integrated tool. Handler errors are returned
// in-band (isError=true) so the model sees the failure as tool output,
// mirroring how MCP tool errors surface.
func (r *Registry) Call(ctx context.Context, display, argsJSON string, meta mcphub.CallMeta) (out string, isErr bool, err error) {
	i, ok := r.byName[display]
	if !ok {
		return "", false, fmt.Errorf("unknown tool %q", display)
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	// An in-process handler (including third-party VM code like the Lua
	// sandbox) must not be able to kill the generation — or the whole
	// process — with a panic; surface it as an in-band tool error instead.
	defer func() {
		if p := recover(); p != nil {
			slog.Error("integrated tool panicked", "tool", display, "panic", p)
			out, isErr, err = "Error: internal tool failure", true, nil
		}
	}()
	out, err = r.tools[i].Handler(ctx, argsJSON, meta)
	if err != nil {
		return "Error: " + err.Error(), true, nil
	}
	return out, false, nil
}
