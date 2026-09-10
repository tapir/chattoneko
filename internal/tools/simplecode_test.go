package tools

import (
	"context"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"chattoneko/internal/mcphub"
)

// callSimpleCode invokes the registered tool through the catalog the same way
// the engine does, returning (result, isError, err).
func callSimpleCode(t *testing.T, argsJSON string) (string, bool, error) {
	t.Helper()
	return Builtin(nil, nil).Call(context.Background(), "simple_code", argsJSON, mcphub.CallMeta{})
}

func TestSimpleCodeBasicArithmetic(t *testing.T) {
	// Lua 5.3+ separates integers from floats: 2^10 is a float, so it prints
	// with the trailing ".0" (Lua 5.2 printed "1024").
	out, isErr, err := callSimpleCode(t, `{"code":"print(1+2*3)\nprint(2^10)\nprint(7//2)"}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if out != "7\n1024.0\n3\n" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestSimpleCodePrintCaptureFormatting(t *testing.T) {
	// Multiple args are tab-separated, each print ends with a newline,
	// and booleans/nil/floats stringify like standard Lua print.
	out, isErr, err := callSimpleCode(t, `{"code":"print(\"hi\", true, nil, 1.5, {1,2})"}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if !strings.HasPrefix(out, "hi\ttrue\tnil\t1.5\t") {
		t.Fatalf("unexpected output: %q", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("output should end with newline: %q", out)
	}
}

func TestSimpleCodeAllowedLibraries(t *testing.T) {
	out, isErr, err := callSimpleCode(t, `{
		"code": "print(string.upper('abc'), table.concat({1,2,3},'-'), math.floor(3.7), bit32.bxor(5,3), type(pcall))"
	}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if out != "ABC\t1-2-3\t3\t6\tfunction\n" {
		t.Fatalf("unexpected output: %q", out)
	}
}

// TestSimpleCodeLibrariesShopifyLacked covers the standard-Lua facilities the
// previous VM did not implement at all: pattern matching, binary packing,
// UTF-8, table.move.
func TestSimpleCodeLibrariesShopifyLacked(t *testing.T) {
	out, isErr, err := callSimpleCode(t, `{
		"code": "print(string.match('a=1','(%w+)=(%d+)'), (string.gsub('xxx','x','y')), utf8.len('héllo'), table.move({1},1,1,2,{9})[2], #string.pack('>i4',7), math.type(3.0))"
	}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if out != "a\tyyy\t5\t1\t4\tfloat\n" {
		t.Fatalf("unexpected output: %q", out)
	}
}

// TestSimpleCodeCoroutinesUnreachable covers why coroutines are removed: each
// one runs on its own goroutine, and an abandoned suspended coroutine costs
// ~16 KB that neither the deadline nor the checkpoint budget bounds, so within
// the budget a snippet can strand ~1.2M of them (~20 GB) and get the process
// OOM-killed. Nil-ing the global alone is not enough — package.loaded hands
// the module straight back through require().
func TestSimpleCodeCoroutinesUnreachable(t *testing.T) {
	before := runtime.NumGoroutine()
	wantOut(t, `print(type(coroutine), type(package.loaded.coroutine))
		local ok, m = pcall(require, 'coroutine')
		print(ok, type(m))`,
		"nil\tnil\nfalse\tstring\n")
	if n := runtime.NumGoroutine(); n > before+5 {
		t.Fatalf("a run spawned %d goroutines", n-before)
	}
}

// TestSimpleCodeSandboxBlocksHostLibraries checks the capability gating: no
// provider is set, so every host-facing module is never registered and
// dofile/loadfile do not exist.
func TestSimpleCodeSandboxBlocksHostLibraries(t *testing.T) {
	out, isErr, err := callSimpleCode(t, `{
		"code": "print(type(io), type(os), type(debug), type(chan), type(time), type(exec), type(http), type(dofile), type(loadfile))"
	}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if out != strings.Repeat("nil\t", 8)+"nil\n" {
		t.Fatalf("sandbox leak — expected all nil, got: %q", out)
	}
}

// TestSimpleCodeRemovedGlobals checks the globals we drop on top of the
// capability gating (see removedGlobals for why each one goes).
func TestSimpleCodeRemovedGlobals(t *testing.T) {
	out, isErr, err := callSimpleCode(t, `{
		"code": "print(type(collectgarbage), type(warn), type(glob), type(_lastoutput), type(_outputlines))"
	}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if out != strings.Repeat("nil\t", 4)+"nil\n" {
		t.Fatalf("unexpected globals: %q", out)
	}
}

// TestSimpleCodePackageIsInert proves require/searchpath/loadlib cannot reach
// the filesystem or dlopen anything without a code/loadlib provider.
func TestSimpleCodePackageIsInert(t *testing.T) {
	// require raises "module not found"; loadlib RETURNS the standard absent
	// triple (nil, errmsg, "absent") instead of raising; searchpath never
	// finds a real file because the searchers only reach disk via a provider.
	out, isErr, err := callSimpleCode(t, `{
		"code": "local ok = pcall(require,'os'); local f,msg,where = package.loadlib('/usr/lib/libc.so.6','printf'); print(ok, type(f), where, msg ~= nil, (package.searchpath('passwd','/etc/?')))"
	}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if out != "false\tnil\tabsent\ttrue\tnil\n" {
		t.Fatalf("package is not inert: %q", out)
	}
}

func TestSimpleCodeLoadIsTextOnly(t *testing.T) {
	// A valid text chunk still compiles and runs via load.
	out, isErr, err := callSimpleCode(t, `{"code":"local f = load('return 6*7'); print(f())"}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if out != "42\n" {
		t.Fatalf("text load broken: %q", out)
	}
}

func TestSimpleCodeLoadRejectsBinary(t *testing.T) {
	// Neither bytecode this VM produced (string.dump) nor hand-crafted bytes
	// carrying the Lua signature may reach the undumper.
	out, isErr, err := callSimpleCode(t, `{"code":"local f,e1 = load(string.dump(load('return 1'))); local g,e2 = load(string.char(27)..'Lua'..'GARBAGE'); print(type(f), e1 ~= nil, type(g), e2 ~= nil)"}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if out != "nil\ttrue\tnil\ttrue\n" {
		t.Fatalf("expected binary rejection, got: %q", out)
	}
}

func TestSimpleCodeLoadAcceptsReaderFunction(t *testing.T) {
	// Apart from the mode, stock load() semantics are preserved: the
	// reader-function form works (golua caps readers at 256 MB / 4M calls).
	out, isErr, err := callSimpleCode(t, `{"code":"local c={'return ','6*7'}; local i=0; local f=load(function() i=i+1; return c[i] end); print(f())"}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if out != "42\n" {
		t.Fatalf("reader load broken: %q", out)
	}
}

func TestSimpleCodeRecursiveMetamethodSafe(t *testing.T) {
	// A self-referential __tostring hits the VM's call-depth cap and must
	// surface as an in-band error, not a host crash.
	out, isErr, err := callSimpleCode(t, `{"code":"local t={}; setmetatable(t,{__tostring=function() return tostring(t) end}); print(tostring(t))"}`)
	if err != nil {
		t.Fatalf("expected in-band error, got Go error: %v", err)
	}
	if !isErr {
		t.Fatalf("expected isError=true for recursive metamethod, out=%q", out)
	}
}

func TestSimpleCodeDeepRecursionIsCatchable(t *testing.T) {
	// Limits.MaxCallDepth turns unbounded recursion into a Lua error the
	// snippet could even pcall — no Go stack exhaustion.
	out, isErr, err := callSimpleCode(t, `{"code":"local function f(n) return 1+f(n) end; print(f(0))"}`)
	if err != nil || !isErr {
		t.Fatalf("expected in-band error: out=%q isErr=%v err=%v", out, isErr, err)
	}
	if !strings.Contains(out, "stack overflow") {
		t.Fatalf("expected a stack overflow error, got: %q", out)
	}
}

func TestSimpleCodeRuntimeErrorSurfacesInBand(t *testing.T) {
	out, isErr, err := callSimpleCode(t, `{"code":"local t=nil; print(t.x)"}`)
	// Handler error -> Registry returns it in-band with isError=true, no Go error.
	if err != nil {
		t.Fatalf("expected in-band error, got Go error: %v", err)
	}
	if !isErr {
		t.Fatalf("expected isError=true, out=%q", out)
	}
	if !strings.Contains(out, "attempt to index") {
		t.Fatalf("expected lua runtime error message, got: %q", out)
	}
}

func TestSimpleCodeSyntaxError(t *testing.T) {
	out, isErr, err := callSimpleCode(t, `{"code":"print(( "}`)
	if err != nil || !isErr {
		t.Fatalf("expected in-band error: out=%q isErr=%v err=%v", out, isErr, err)
	}
	if !strings.Contains(out, "unexpected symbol") {
		t.Fatalf("expected a parse error, got: %q", out)
	}
	// The chunk name is reported instead of echoing the snippet back.
	if !strings.Contains(out, chunkName) {
		t.Fatalf("expected the chunk name in the error, got: %q", out)
	}
}

func TestSimpleCodeInfiniteLoopAborted(t *testing.T) {
	// A `while true do end` burns the checkpoint budget in well under the
	// context deadline and returns an error instead of hanging the host.
	start := time.Now()
	_, err := runLua(context.Background(), "while true do end")
	if err == nil {
		t.Fatal("expected an instruction-limit error for an infinite loop")
	}
	if !strings.Contains(err.Error(), "instruction limit exceeded") {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("runaway loop took %v", elapsed)
	}
}

// TestSimpleCodeRunawayPrintLoopBounded covers the reason the checkpoint
// budget exists at all: the capture buffer is an uncapped append, so a
// deadline alone would let a print loop retain hundreds of MB.
func TestSimpleCodeRunawayPrintLoopBounded(t *testing.T) {
	start := time.Now()
	_, err := runLua(context.Background(), "while true do print(1) end")
	if err == nil {
		t.Fatal("expected an instruction-limit error for a runaway print loop")
	}
	if !strings.Contains(err.Error(), "instruction limit exceeded") {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("runaway print loop took %v", elapsed)
	}
}

func TestSimpleCodeCancelledContextAborts(t *testing.T) {
	// The VM runs under the handler's context, so a cancelled turn stops it —
	// something a debug count-hook alone could not do.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := runLua(ctx, "while true do end")
	if err == nil {
		t.Fatal("expected a context error for a cancelled context")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("cancelled run took %v", elapsed)
	}
}

func TestSimpleCodeBoundedLoopUnderDefaultBudget(t *testing.T) {
	// A legit bounded loop must run fine under the production budget.
	out, err := runLua(context.Background(), "local s=0; for i=1,100000 do s=s+i end; print(s)")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out != "5000050000\n" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestSimpleCodeOutputTruncated(t *testing.T) {
	// 20k lines x 100 bytes = ~2 MB captured, well under the checkpoint
	// budget, so the run succeeds and the RESULT is what gets capped.
	out, isErr, err := callSimpleCode(t, `{"code":"for i=1,20000 do print(string.rep('x',100)) end"}`)
	if err != nil || isErr {
		t.Fatalf("out(len)=%d isErr=%v err=%v", len(out), isErr, err)
	}
	if len(out) > maxOutputBytes+256 {
		t.Fatalf("result not capped: %d bytes", len(out))
	}
	if !strings.Contains(out, "output truncated") {
		t.Fatalf("expected a truncation notice, got tail: %q", out[len(out)-120:])
	}
}

func TestSimpleCodeNoPrintProducesGuidance(t *testing.T) {
	out, isErr, err := callSimpleCode(t, `{"code":"local x=42"}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if !strings.Contains(out, "printed nothing") {
		t.Fatalf("expected no-output guidance, got: %q", out)
	}
}

func TestSimpleCodeArgumentValidation(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string // substring expected in the in-band error
	}{
		{"invalid json", `{not json`, "invalid arguments JSON"},
		{"missing code", `{}`, "code is required"},
		{"empty code", `{"code":"   "}`, "code is required"},
		{"oversized code", `{"code":"` + strings.Repeat("x", maxCodeBytes+1) + `"}`, "code too large"},
	}
	for _, tc := range cases {
		out, isErr, err := callSimpleCode(t, tc.args)
		if err != nil {
			t.Fatalf("%s: expected in-band error, got Go error: %v", tc.name, err)
		}
		if !isErr {
			t.Fatalf("%s: expected isError=true", tc.name)
		}
		if !strings.Contains(out, tc.want) {
			t.Fatalf("%s: want %q in %q", tc.name, tc.want, out)
		}
	}
}

// TestSimpleCodeDoesNotLeakToStdout proves print() is captured in-memory: the
// VM's fallback when nothing captures output is os.Stdout, so a missing
// WithCaptureOutput would show up as bytes on stdout here.
func TestSimpleCodeDoesNotLeakToStdout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	os.Stdout = w

	_, _ = runLua(context.Background(), "print('leak-check')")

	_ = w.Close()
	os.Stdout = oldStdout
	captured, _ := io.ReadAll(r)
	_ = r.Close()

	if len(captured) != 0 {
		t.Fatalf("print() leaked to stdout: %q", captured)
	}
}
