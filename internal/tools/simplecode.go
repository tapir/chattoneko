package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	lua "github.com/Shopify/go-lua"

	"chattoneko/internal/mcphub"
)

// The "simple_code" tool: runs a small Lua snippet in a restricted sandbox
// and returns whatever the snippet prints. It exists so the model can do
// exact arithmetic, string/data wrangling, or logic that is error-prone to
// do "in its head" — an advanced calculator / expression evaluator.
//
// SANDBOXING — only five libraries are loaded into the Lua state:
//
//	base   (print, pcall, tostring, type, pairs, ipairs, ...)
//	string
//	table
//	math
//	bit32
//
// The io, os, package, coroutine and debug libraries are deliberately NOT
// opened, so the snippet has no file, network, environment, or debug access.
// The allowed set is hardcoded below in sandboxLibraries; the LLM-facing
// description repeats it so the model knows what it can rely on.
//
// HARDENING — a few base-library globals are unsafe in this context and are
// neutralized after the libraries are opened (see hardenSandbox):
//
//   - dofile()/loadfile() read arbitrary files from the server's filesystem;
//     both are removed.
//   - load() in its default "bt" mode also parses precompiled binary
//     bytecode, which is a parser over attacker-controlled bytes. It is
//     replaced with a text-only wrapper (mode "t") that keeps the useful
//     string-compilation behavior but rejects binary chunks.
//   - collectgarbage() calls the host's runtime.GC() (a stop-the-world
//     pause that a snippet could hammer) and leaks heap statistics; it is
//     removed.
//
// Everything else in base (print, pcall, tostring, type, pairs, ipairs,
// load-as-text, ...) is kept.
//
// GETTING RESULTS BACK — the tool returns ONLY what the snippet sends to
// print(). We do not read return values off the Lua stack (we cannot know
// what the model will compute), so the description tells the model to always
// print() its final result; the return value of the last expression is
// discarded. To make this work, the standard print() is replaced with a Go
// function that appends to a buffer instead of writing to the process
// stdout (which the go-lua default print does directly).
//
// SAFETY — the snippet is executed exclusively through lua.DoString (which
// itself uses a protected call), so a Lua error is returned as a Go error
// rather than crashing the host. Two further limits keep a hostile or
// runaway snippet from wedging the turn loop:
//
//   - an instruction budget enforced by a debug count-hook, which aborts an
//     infinite loop without ever touching os.Exit/panic in the host, and
//   - an output cap, so a snippet that prints in a tight loop cannot grow an
//     unbounded result string.
//
// All user/LLM-facing text is hardcoded here — edit in place to change it.

// maxCodeBytes bounds the incoming snippet size. Snippets are meant to be
// small; this keeps a pathological payload from even reaching the VM.
const maxCodeBytes = 64 * 1024

// luaInstructionBudget bounds total executed VM instructions. It is generous
// enough for any realistic calculation but aborts a `while true do end`
// within a fraction of a second (this VM runs ~100M instructions/sec).
const luaInstructionBudget = 100_000_000

// luaHookBatch is how many instructions pass between count-hook firings.
// Coarse batching keeps hook overhead negligible; the effective ceiling is
// budget ± batch, which is fine for a runaway guard.
const luaHookBatch = 10_000

// maxOutputBytes caps captured print output. Exceeding it aborts the run
// with a clear error rather than producing a multi-megabyte tool result.
const maxOutputBytes = 1 * 1024 * 1024

// The code argument is a single required string.
var simpleCodeSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"code": {
			"type": "string",
			"description": "Complete Lua 5.2 snippet. Use print(...) to emit results — anything not printed is discarded. Example: print(2^10 + math.floor(3.7))."
		}
	},
	"required": ["code"],
	"additionalProperties": false
}`)

var SimpleCode = Tool{
	Name: "simple_code",
	Description: "Run a short Lua 5.2 snippet in a restricted sandbox and get back whatever it prints. " +
		"Use it as an advanced calculator / expression evaluator for exact arithmetic, date/duration math, " +
		"string manipulation, or table/data processing that you should not do in your head. " +
		"Available libraries: base (print, pcall, tostring, type, pairs, ipairs, ...), string, table, math, bit32. " +
		"There is NO access to files, the network, the OS, or the environment. " +
		"IMPORTANT: the sandbox returns only what you send to print() — always print(...) your final result; " +
		"return values are discarded. Execution is instruction-capped, so keep loops reasonable.",
	Schema:         simpleCodeSchema,
	DefaultEnabled: true,
	Handler:        simpleCode,
}

func simpleCode(_ context.Context, argsJSON string, _ mcphub.CallMeta) (string, error) {
	var args struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments JSON: %v", err)
	}
	code := strings.TrimSpace(args.Code)
	if code == "" {
		return "", errors.New("code is required")
	}
	if len(code) > maxCodeBytes {
		return "", fmt.Errorf("code too large (max %d bytes)", maxCodeBytes)
	}
	return runLua(code, luaInstructionBudget)
}

// sandboxLibraries is the whitelist of libraries opened in the Lua state.
// Anything not listed here is simply absent from the VM.
var sandboxLibraries = []struct {
	name string
	open lua.Function
}{
	{"_G", lua.BaseOpen},
	{"string", lua.StringOpen},
	{"table", lua.TableOpen},
	{"math", lua.MathOpen},
	{"bit32", lua.Bit32Open},
}

// newSandboxState builds a fresh Lua state with only the whitelisted
// libraries loaded. A new state per call keeps executions fully isolated.
// Filesystem-touching base globals (dofile, loadfile) are then removed.
func newSandboxState() *lua.State {
	l := lua.NewState()
	for _, lib := range sandboxLibraries {
		lua.Require(l, lib.name, lib.open, true)
		l.Pop(1) // Require leaves the module table on the stack
	}
	hardenSandbox(l)
	return l
}

// hardenSandbox neutralizes base-library globals that would otherwise defeat
// the sandbox. Called after the libraries are opened.
func hardenSandbox(l *lua.State) {
	// dofile/loadfile read server files; collectgarbage drives the host GC
	// and leaks heap stats. None belong in a calculator sandbox.
	for _, name := range []string{"dofile", "loadfile", "collectgarbage"} {
		l.PushNil()
		l.SetGlobal(name)
	}
	restrictLoadToText(l)
}

// restrictLoadToText replaces the base load() with a wrapper that only
// compiles literal string chunks as TEXT (mode "t"). The stock load()
// defaults to mode "bt", which additionally parses precompiled binary
// bytecode via undump; allowing that would mean feeding attacker-controlled
// bytes into the binary parser. The function-reader form of load() is also
// rejected for simplicity — string chunks are all a calculator needs.
func restrictLoadToText(l *lua.State) {
	l.PushGoFunction(func(s *lua.State) int {
		chunk, ok := s.ToString(1)
		if !ok {
			s.PushNil()
			s.PushString("load: only literal string chunks are supported")
			return 2
		}
		name := chunk
		if n, ok := s.ToString(2); ok && n != "" {
			name = n
		}
		if err := lua.LoadBuffer(s, chunk, name, "t"); err != nil {
			// On error LoadBuffer already pushed the message string; add a
			// nil before it so the result matches load()'s (nil, msg) contract.
			s.PushNil()
			s.Insert(-2)
			return 2
		}
		// On success the compiled function is already on top of the stack.
		return 1
	})
	l.SetGlobal("load")
}

// overridePrint replaces the base library's print with a Go function that
// appends to out instead of writing to the process stdout. It mimics the
// standard print: each argument is stringified (honoring __tostring),
// arguments are separated by tabs, and each call ends with a newline. If the
// output cap is exceeded it raises a Lua error to stop the run cleanly.
func overridePrint(l *lua.State, out *strings.Builder) {
	l.PushGoFunction(func(s *lua.State) int {
		n := s.Top()
		for i := 1; i <= n; i++ {
			str, ok := lua.ToStringMeta(s, i)
			if !ok {
				str = "<unprintable>"
			}
			if i > 1 {
				out.WriteString("\t")
			}
			out.WriteString(str)
			s.Pop(1) // pop the stringified value ToStringMeta pushed
		}
		out.WriteString("\n")
		if out.Len() > maxOutputBytes {
			lua.Errorf(s, "output too large (exceeded %d bytes); print less", maxOutputBytes)
		}
		return 0
	})
	l.SetGlobal("print")
}

// setInstructionLimit installs a count-hook that aborts the run once the
// instruction budget is spent. The abort is raised as a Lua error, so it is
// caught by DoString's protected call and surfaced as a normal error — the
// host never crashes. budget <= 0 disables the limit.
func setInstructionLimit(l *lua.State, budget int) {
	if budget <= 0 {
		return
	}
	remaining := budget
	lua.SetDebugHook(l, func(s *lua.State, _ lua.Debug) {
		remaining -= luaHookBatch
		if remaining <= 0 {
			lua.Errorf(s, "instruction budget exceeded (possible infinite loop)")
		}
	}, lua.MaskCount, luaHookBatch)
}

// runLua executes code in a fresh sandbox and returns everything it printed.
// budget bounds executed VM instructions (0 disables the limit). A Lua
// compile/runtime error is returned as an error (which the Registry surfaces
// to the model in-band as "Error: ...").
func runLua(code string, budget int) (string, error) {
	l := newSandboxState()
	var out strings.Builder
	overridePrint(l, &out)
	setInstructionLimit(l, budget)

	if err := lua.DoString(l, code); err != nil {
		return "", err
	}
	if out.Len() == 0 {
		return "(ran successfully but printed nothing — use print(...) to emit results)", nil
	}
	return out.String(), nil
}
