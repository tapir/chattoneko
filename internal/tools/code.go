package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/iceisfun/golua/compiler"
	"github.com/iceisfun/golua/parser"
	"github.com/iceisfun/golua/stdlib"
	"github.com/iceisfun/golua/vm"

	"chattoneko/internal/mcphub"
)

// The "code" tool: runs a small Lua snippet in a restricted sandbox
// and returns whatever the snippet prints. It exists so the model can do
// exact arithmetic, string/data wrangling, or logic that is error-prone to
// do "in its head" — an advanced calculator / expression evaluator.
//
// The VM is golua v1 (Lua 5.4.8). Sandboxing is capability-based rather than a
// hand-picked library whitelist: stdlib.Open registers the standard modules,
// and every host-facing one only appears when its provider is set. We set no
// providers, so io, os, debug, chan, time, exec and http are never
// registered at all, and dofile/loadfile do not exist without a code
// provider. What is left is base plus string, table, math, bit32, utf8, an
// inert package/require (its searchers only reach the filesystem through a
// code provider, and package.loadlib always reports "absent") and our own
// json module (luajson.go).
//
// WHY v1 AND NOT v2 — golua ships two maintained branches in lockstep (same
// release days, same provider/sandbox/limits API, so the only difference here
// is the language version) and /v2 is Lua 5.5. Lua 5.5 makes for-loop control
// variables read-only, which rejects code every model writes from 5.1-5.4
// memory — `for w in s:gmatch(...) do w = w:gsub(...) end` is a compile error
// there, so the whole snippet dies before running. This tool is a calculator
// for an LLM, not a place to adopt a language version no model has seen; v1
// keeps the 5.1/5.2 compat aliases (math.pow, bit32, ...) so those idioms run
// too. Bumping to /v2 is an import-path change plus this description's
// version text — and TestCodeLua54Semantics below will fail first.
//
// hardenSandbox then drops the few globals that do not belong here — see
// removedGlobals for why each one goes, and note that dropping a module also
// has to clear its package.loaded entry or require() hands it straight back —
// and replaces load() with a text-only wrapper so no precompiled bytecode
// reaches the undumper.
//
// GETTING RESULTS BACK — the tool returns ONLY what the snippet sends to
// print(), captured in-memory by the VM (vm.WithCaptureOutput) rather than
// written to the process stdout; stdlib does the tab-joining and honors
// __tostring. We do not read return values off the VM (we cannot know what
// the model will compute), so the description tells the model to always
// print() its final result.
//
// LIMITS — a snippet is bounded three ways, none of them hand-rolled:
//
//   - wall clock: the VM runs under the handler's context, which
//     registry.Call already bounds (30s). golua checks cancellation at loop
//     backedges, calls and tail calls, so a runaway loop is aborted —
//     something a debug count-hook alone cannot do.
//   - work/memory: luaCheckpointBudget. A deadline bounds CPU but not
//     memory, and the capture buffer is an uncapped append; this is what
//     stops a print loop from retaining hundreds of MB before the deadline
//     lands.
//   - output: the returned result is truncated at maxOutputBytes, so a
//     chatty snippet cannot produce a multi-megabyte tool result.
//
// Lua errors (including the limit errors, which are catchable by pcall) come
// back as a Go error, which the registry surfaces to the model in-band as
// "Error: ..." — the host never crashes.
//
// All user/LLM-facing text is hardcoded here — edit in place to change it.

// maxCodeBytes bounds the incoming snippet size. Snippets are meant to be
// small; this keeps a pathological payload from even reaching the VM.
const maxCodeBytes = 1 << 20 // 1 MiB

// chunkName is what Lua error messages report as the source of the snippet
// ("[string \"code\"]:1: ...") instead of echoing the code itself.
const chunkName = "code"

// luaCheckpointBudget bounds VM checkpoints — loop backedges, calls and tail
// calls, NOT raw instructions. Wall-clock time is already bounded by the
// handler's context; this bound exists because a deadline cannot bound
// memory. golua's output capture appends without limit, so a runaway print
// loop retains ~450 MB/s until the context deadline lands. 5M checkpoints
// caps that at roughly 2.5M captured lines while leaving ~50x the headroom
// any realistic calculation needs.
//
// ponytail: this bounds iterations, not bytes per iteration, so retention is
// still only bounded by the deadline — a loop keeping string.rep results
// grows at ~400 MB/s, i.e. ~12 GB across the 30s window. golua caps each
// string at 1 GB and clamps table growth into a catchable "not enough
// memory", but nothing caps how many live strings a run may hold, and
// debug.SetMemoryLimit would only turn the OOM kill into a fatal throw.
// Upgrade path if this ever faces untrusted users: run the VM in a subprocess
// under RLIMIT_AS or a cgroup memory.max.
const luaCheckpointBudget = 5_000_000

// maxOutputBytes caps the returned tool result. The checkpoint budget bounds
// what a run can capture, but that is still far more than belongs in a tool
// result the model has to read.
const maxOutputBytes = 1 * 1024 * 1024

// The code argument is a single required string.
var codeSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"code": {
			"type": "string",
			"description": "Complete Lua 5.4 snippet. Use print(...) to emit results — anything not printed is discarded. Example: print(2^10 + math.floor(3.7))."
		}
	},
	"required": ["code"],
	"additionalProperties": false
}`)

var Code = tool{
	Name: "code",
	Description: `Run Lua 5.4 in a sandbox. Results come back ONLY through print(): return values are discarded.
print() tab-joins its args, a table prints as "table: 0x..." — serialize with json.encode or table.concat.
One print = one line; print a summary, not a dump.

Restricted: no io, os, debug, coroutine, files, network, env, threads, or clock.
Get the current time from the time tool and do date math yourself (86400 s/day, mind leap years and month lengths).

Numbers (5.3): integers are 64-bit and wrap (maxint+1 = minint).
/ always returns float (7/2 = 3.5); // floors and keeps operand type; % is Lua's modulo (a - floor(a/b)*b),
so -7 % 3 = 2 but math.fmod(-7,3) = -1. Floats always print with a decimal/exponent and 14 sig figs:
0.1+0.2 prints "0.3" yet != 0.3. Converting an int above 2^53 to float loses precision.
Bitwise & | ~ << >> need integer values (convert with x//1); ~ is bitwise NOT, ~= is inequality.
string.format("%d", x) needs an integer-valued x. math.floor/ceil return integers; math.tointeger and math.type exist.

Patterns are NOT regex — backslash escapes like \d are compile errors. Use %d, %s, %a; - is lazy;
%b xy matches balanced, %f[set] frontier; find/match/gmatch/gsub take an optional init index, and find accepts
a plain flag. gsub returns (result, count); load returns (chunk, errmsg). A gsub table replacement keyed by the
match keeps the original if its value is nil/false.

Tables: # is a border, not a count — unreliable with holes, so use table.pack(...).n or select('#',...).
table.concat raises on a nil hole. table.sort comparator must be strict weak ordering: use < never <=.
pairs() order is unspecified; ipairs stops at first nil.

JSON (custom module): json.encode(value) -> string, json.decode(string) -> value. A table is a JSON array only when keys are exactly 1..n (empty table = {}, {1,2,x=3} =
{"1":..,"2":..,"x":..}); object keys come out sorted. JSON null decodes to nil and REMOVES that key from its table.
Both raise on malformed input — wrap them in pcall.`,
	Schema:         codeSchema,
	DefaultEnabled: true,
	Title:          "Coding…",
	Handler:        runCode,
}

func runCode(ctx context.Context, argsJSON string, _ mcphub.CallMeta) (string, error) {
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
	return runLua(ctx, code)
}

// removedGlobals are dropped after stdlib.Open. Everything else the sandbox
// lacks (io, os, debug, chan, time, exec, http, dofile, loadfile) is absent
// because no provider was set — capability gating, not deletion.
var removedGlobals = []string{
	// Each Lua coroutine runs on its own goroutine, and one left suspended
	// and abandoned costs ~16 KB that neither the deadline nor the
	// checkpoint budget bounds — the budget counts iterations, not bytes,
	// and a coroutine is ~180x what a printed line costs. Measured: within
	// the budget a snippet strands ~1.2M of them (~20 GB) and the kernel
	// OOM-kills the process. v.Close reaps them, but only after the run.
	"coroutine",
	// Drives the host's garbage collector, and collectgarbage("count")
	// reports the whole Go process's heap in KB. runtime.ReadMemStats is
	// also a stop-the-world pause, so a loop over it measurably slows down
	// every other goroutine in the process until the deadline lands.
	"collectgarbage",
	// WithCaptureOutput captures print() only; warn() would write straight
	// to the host's stderr.
	"warn",
	// golua extensions, not standard Lua: Go-style globbing, plus helpers
	// for inspecting the host-side capture buffer from inside the sandbox.
	"glob", "_lastoutput", "_outputlines",
}

// removeGlobal drops a global and its package.loaded entry. Clearing only the
// global is not enough: openPackage populates package.loaded from the module
// globals at the end of stdlib.Open, so `require("coroutine")` would hand the
// module straight back.
func removeGlobal(v *vm.VM, name string) {
	v.SetGlobal(name, vm.Nil)
	pkg := v.GetGlobal("package")
	if !pkg.IsTable() {
		return
	}
	if loaded := pkg.AsTable().Get(vm.NewString("loaded")); loaded.IsTable() {
		loaded.AsTable().Set(vm.NewString(name), vm.Nil)
	}
}

// runLua executes code in a fresh sandbox and returns everything it printed.
// The run is bounded by ctx (registry.Call gives every integrated tool a 30s
// deadline) and by luaCheckpointBudget. A new VM per call keeps executions
// fully isolated; there is no shared state to reset.
func runLua(ctx context.Context, code string) (string, error) {
	block, err := parser.Parse(chunkName, code)
	if err != nil {
		return "", err
	}
	proto, err := compiler.Compile(chunkName, block)
	if err != nil {
		return "", err
	}

	v := vm.New(
		vm.WithContext(ctx),
		vm.WithCaptureOutput(true),
		vm.WithLimits(vm.Limits{
			MaxInstructions: luaCheckpointBudget,
			MinGCInterval:   -1, // Lua must never trigger the host's GC
		}),
	)
	stdlib.Open(v)
	for _, name := range removedGlobals {
		removeGlobal(v, name)
	}
	textOnlyLoad(v)
	openJSON(v)

	// The documented VM lifecycle: Close is what reaps goroutines a library
	// spawned. Coroutines are removed today, so there is nothing to reap.
	defer v.Close(ctx)

	if _, err := v.Run(proto); err != nil {
		return "", err
	}
	return joinOutput(v.OutputLines()), nil
}

// textOnlyLoad keeps stock load() semantics (reader functions, chunkname,
// env) but forces mode "t", so precompiled bytecode never reaches the
// undumper. golua validates that parser and recovers its panics, but load()
// defaults to mode "bt" and a snippet has no legitimate reason to feed bytes
// to it — an LLM emits source, not bytecode.
func textOnlyLoad(v *vm.VM) {
	stock := v.GetGlobal("load")
	v.SetGlobal("load", vm.NewNativeFunc(func(v *vm.VM) int {
		args := []vm.Value{v.Get(1), v.Get(2), vm.NewString("t")}
		if v.ArgCount() >= 4 {
			args = append(args, v.Get(4))
		}
		res, err := v.ProtectedCall(stock, args)
		if err != nil {
			v.Set(0, vm.Nil)
			v.Set(1, vm.NewString(err.Error()))
			return 2
		}
		for i, r := range res {
			v.Set(i, r)
		}
		return len(res)
	}))
}

// joinOutput renders the captured print lines into the tool result. The VM
// hands back one newline-free string per print() call (arguments already
// tab-joined, __tostring already honored), so this only adds the newlines and
// enforces maxOutputBytes.
func joinOutput(lines []string) string {
	if len(lines) == 0 {
		return "(ran successfully but printed nothing — use print(...) to emit results)"
	}
	out := strings.Join(lines, "\n") + "\n"
	if len(out) > maxOutputBytes {
		// ponytail: the cut can split a multi-byte rune; encoding/json
		// replaces the stray bytes with U+FFFD on the way to the model.
		out = out[:maxOutputBytes] +
			fmt.Sprintf("\n(output truncated at %d bytes — print less)\n", maxOutputBytes)
	}
	return out
}
