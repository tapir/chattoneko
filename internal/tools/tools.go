// Package tools provides the integrated (built-in) tool catalog: tools
// implemented in-process and declared entirely in code. Integrated tools
// implement the same engine.ToolCatalog interface as the MCP hub, so the
// engine's turn loop, per-chat toggles and the API's tool listing treat them
// identically to MCP tools.
//
// A new integrated tool is a `var MyTool = tool{...}` in its own file here,
// listed in Builtin() (catalog.go).
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"chattoneko/internal/mcphub"
)

// callTimeout bounds one integrated tool call so a handler doing I/O cannot
// block the turn loop indefinitely.
const callTimeout = 30 * time.Second

// tool is one integrated tool definition; all of its LLM-facing text lives in
// the tool's own file. DefaultEnabled is the starting state for chats that
// have not toggled the tool explicitly (per-chat overrides live in the chat's
// persisted tools map).
type tool struct {
	Name           string          // LLM-facing name (must be unique across the whole catalog)
	Description    string          // LLM-facing description
	Schema         json.RawMessage // JSON schema for the arguments
	DefaultEnabled bool            // enabled unless the chat or the config says otherwise
	// Title is the user-facing label the chat UI shows instead of Name
	// ("Coding…"); the model never sees it. Empty falls back to Name in the UI.
	Title string
	// Modality is the chat-model input modality that makes the tool pointless,
	// because the model takes that input itself ("image" for vision). Empty for
	// every other tool, which is always offered. The engine leaves such a tool
	// out of the request and the chat UI greys it out.
	Modality string
	// Timeout bounds one call, overriding callTimeout. Only a handler doing
	// remote I/O needs it (the specialists wait on another model); 0 keeps the
	// default.
	Timeout time.Duration
	Handler handler
}

// handler executes one tool call. argsJSON is the raw arguments JSON the
// model produced ("" when the model sent none); handlers that take arguments
// should json.Unmarshal and validate them themselves. meta carries the
// chat/message coordinates of the call for handlers that persist artifacts
// (e.g. create_file showing a file on the assistant message that asked for it).
// The returned string is what the model sees as the tool result.
type handler func(ctx context.Context, argsJSON string, meta mcphub.CallMeta) (string, error)

// registry is a static catalog of integrated tools. It implements
// engine.ToolCatalog.
type registry struct {
	tools  []tool
	byName map[string]int // name → index into tools
}

// newRegistry builds a registry from the given tool definitions. A missing
// name or handler and a duplicate name panic at startup.
func newRegistry(ts ...tool) *registry {
	r := &registry{byName: map[string]int{}}
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
func (r *registry) Tools() []mcphub.Entry {
	out := make([]mcphub.Entry, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, mcphub.Entry{
			Display:        t.Name,
			Description:    t.Description,
			Server:         mcphub.BuiltinServer,
			Schema:         t.Schema,
			DefaultEnabled: t.DefaultEnabled,
			Title:          t.Title,
			Modality:       t.Modality,
		})
	}
	return out
}

// Call invokes the named integrated tool. Handler errors are returned in-band
// (isError=true) so the model sees the failure as tool output, mirroring how
// MCP tool errors surface.
func (r *registry) Call(ctx context.Context, display, argsJSON string, meta mcphub.CallMeta) (out string, isErr bool, err error) {
	i, ok := r.byName[display]
	if !ok {
		return "", false, fmt.Errorf("unknown tool %q", display)
	}
	timeout := callTimeout
	if t := r.tools[i].Timeout; t > 0 {
		timeout = t
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// An in-process handler (including third-party VM code like the Lua
	// sandbox) must not be able to kill the generation or the whole process
	// with a panic; surface it as an in-band tool error instead.
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
