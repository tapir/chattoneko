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
//     Registry.Call already bounds (30s). golua checks cancellation at loop
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
// back as a Go error, which the Registry surfaces to the model in-band as
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

var Code = Tool{
	Name: "code",
	Description: "Run a short Lua 5.4 snippet in a restricted sandbox and get back whatever it prints. " +
		"Use it as an advanced calculator / expression evaluator for work that is error-prone to do in your head: " +
		"exact arithmetic, date and duration math on epoch seconds, text processing with Lua patterns, base conversions, table and data wrangling.\n\n" +

		"GETTING RESULTS OUT — read this first. The result is ONLY what you pass to print(); return values are discarded. " +
		"print() tab-joins its arguments and applies tostring, so a table prints as 'table: 0x...' — serialize it yourself with json.encode or table.concat. " +
		"One print() is one line, and the result is truncated at 1 MiB, so print a summary rather than a dump.\n\n" +

		"AVAILABLE. Globals: assert, error, pcall, xpcall, getmetatable, setmetatable, rawget, rawset, rawequal, rawlen, " +
		"ipairs, next, pairs, print, select, tonumber, tostring, type, load, require (inert), _G, _VERSION. " +
		"Libraries: string, table, math, utf8, bit32 and json (ours).\n" +
		"NOT AVAILABLE — these are nil, not gated: io, os, debug, coroutine, warn, collectgarbage, dofile, loadfile, " +
		"and require() cannot load any module. No files, no network, no environment, no threads. " +
		"load() accepts text only (string.dump exists but its bytecode cannot be loaded back). " +
		"There is no clock in here: get the current date and time from the time tool, then do the arithmetic yourself " +
		"(86400 seconds per day, and mind leap years and month lengths).\n\n" +

		"LUA 5.1 -> 5.4: REMOVED OR RENAMED. Snippets written from 5.1/LuaJIT memory fail on these — " +
		"setfenv/getfenv -> _ENV (a plain assignable upvalue); module()/package.seeall -> return a table; " +
		"unpack -> table.unpack; loadstring -> load; table.getn -> #; table.maxn -> select('#',...) or table.pack(...).n; " +
		"table.foreach/table.foreachi -> pairs/ipairs; string.gfind -> string.gmatch; math.mod -> math.fmod; " +
		"newproxy -> gone; the __ipairs metamethod is ignored by ipairs.\n" +
		"STILL PRESENT HERE, although stock Lua removed them in 5.3: math.pow, math.atan2, math.log10, " +
		"math.cosh/sinh/tanh, math.frexp, math.ldexp and the whole bit32 library, so those older idioms do run — " +
		"but prefer ^, math.atan(y,x), math.log(x,10) and the native bitwise operators.\n\n" +

		"INTEGERS AND FLOATS (5.3, the largest change). Two numeric types: math.type(x) returns 'integer', 'float' or nil, " +
		"and integers are 64-bit and wrap, so math.maxinteger+1 == math.mininteger. " +
		"'/' always yields a float (7/2 == 3.5, and 2^10 prints as 1024.0); '//' floors and keeps the operand type " +
		"(7//2 == 3 but 7.0//2 == 3.0); integer '//' by zero raises 'attempt to divide by zero' while float division by zero " +
		"gives inf or -inf at runtime, though the literal 1/0 is a compile error. " +
		"'%' is a - floor(a/b)*b, so -7 % 3 == 2 while math.fmod(-7,3) == -1. " +
		"A float always prints with a decimal point or exponent (tostring(3.0) == '3.0') and with 14 significant digits: " +
		"0.1+0.2 prints as 0.3 yet (0.1+0.2) == 0.3 is false — use string.format('%.17g', x) when you need the exact value. " +
		"Converting above 2^53 loses precision: tostring(math.maxinteger) is exact, tostring(math.maxinteger + 0.0) is not, " +
		"and the two compare unequal. " +
		"Bitwise &, |, ~, << and >> only accept numbers with an exact integer value — 3.5 & 3 raises " +
		"'number has no integer representation' while 5.0 & 3 is fine, so convert first with x//1 or math.tointeger; " +
		"unary ~ is bitwise NOT and inequality is ~=. " +
		"string.format('%d', x) needs an integer-valued number ('%d' on 3.5 raises). " +
		"math.floor and math.ceil return integers while math.sqrt always returns a float. " +
		"New in 5.3/5.4: math.tointeger, math.maxinteger, math.mininteger, math.ult, math.type; " +
		"math.random(m,n) returns an integer for integer bounds, and math.randomseed(n) makes a run reproducible.\n\n" +

		"NEW SINCE 5.1 AND WORTH USING. goto with ::label:: (5.2); the <const> and <close> variable attributes (5.4) — " +
		"assigning to a <const> is a compile error, which pcall cannot catch; " +
		"string.pack/unpack/packsize (5.3) with formats like '>i4', '<I2', 'd', 'j', 'z', 's8', where a leading >, < or = sets " +
		"endianness and the variable-length 'z'/'sN' forms are rejected by packsize; " +
		"the utf8 library (5.3): utf8.len, char, codepoint, codes, offset, charpattern — # and string.sub count BYTES, " +
		"so #('héllo') == 6; table.move and table.pack/unpack (5.2/5.3); rawlen (5.2); " +
		"xpcall(f, msgh, args...) passes extra arguments (5.2); string.rep(s, n, sep) (5.2); " +
		"hexadecimal float literals such as 0x1p4 == 16.0 (5.3); numeric for-loops no longer wrap at math.maxinteger (5.4).\n\n" +

		"PATTERNS ARE NOT REGEX. A regex escape such as \\d is a compile error ('invalid escape sequence'). " +
		"Classes are %a %c %d %g %l %p %s %u %w %x, uppercased to negate (%D is a non-digit). " +
		"Quantifiers: * greedy, +, - LAZY (Lua's only non-greedy one), ?. Anchors ^ and $; captures (...) plus the position " +
		"capture (); %b xy matches a balanced pair; %f[set] is a frontier match. " +
		"find/match/gmatch/gsub accept an optional init position, and find takes a plain flag — s:find('.', 1, true) for a " +
		"literal dot. A gsub replacement may be a string with %0..%9, a table keyed by the match (a nil or false value keeps " +
		"the original text), or a function. " +
		"gsub returns TWO values (result, count) and load returns (chunk, errmsg): as the LAST argument to print you see both, " +
		"anywhere else in an expression list a call truncates to one value, so print('x' .. ('aaa'):gsub('a','b')) prints 'xbbb'.\n\n" +

		"TABLES. '#' is a border, not a count — #{1,nil,3} may be 3 or 1, so never rely on it when there are holes; " +
		"use select('#',...) or table.pack(...).n instead. pairs() order is unspecified. ipairs() stops at the first nil. " +
		"table.concat raises on a nil hole. table.sort's comparator must be a strict weak ordering: returning true for equal " +
		"elements can raise 'invalid order function for sorting', so use < and never <=. " +
		"Only a table whose keys are exactly 1..n is an array to json.encode; anything else is an object.\n\n" +

		"JSON (our module). json.encode(value) -> string, json.decode(text) -> value. " +
		"encode takes ANY value, not just tables: json.encode(42) is 42, json.encode('x') is \"x\", json.encode(nil) is null. " +
		"A table becomes a JSON array only when its keys are exactly 1..n, so an empty table encodes as {} and {1,2,x=3} as " +
		"{\"1\":1,\"2\":2,\"x\":3}; object keys come out sorted, and a NaN or infinite float is an error. " +
		"decode keeps integers exact to int64, and JSON null becomes nil — which REMOVES that key from its table, " +
		"so {\"a\":null} decodes to an empty table and [1,null,3] leaves a hole at index 2. " +
		"Both RAISE a Lua error on bad input (malformed JSON, NaN, input over 16 MB) instead of returning nil, " +
		"so wrap them in pcall when the text may not be valid JSON.\n\n" +

		"LIMITS AND ERRORS. 1 MiB of code, a 30 s wall clock, about 5M checkpoints (loop backedges and calls — a few " +
		"million iterations of a simple loop) and 1 MiB of returned output. " +
		"Errors arrive in-band as 'Error: [string \"code\"]:LINE: message' and abort the snippet; a syntax error means " +
		"nothing ran at all. pcall and xpcall catch runtime errors (including the 'stack overflow' of deep recursion) but " +
		"never a syntax error in the chunk you submitted — for code you build at runtime use load(text) and check its " +
		"second return value.",
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
// The run is bounded by ctx (Registry.Call gives every integrated tool a 30s
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
