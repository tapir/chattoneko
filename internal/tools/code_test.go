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

// callCode invokes the registered tool through the catalog the same way
// the engine does, returning (result, isError, err).
func callCode(t *testing.T, argsJSON string) (string, bool, error) {
	t.Helper()
	return Builtin(nil, nil).Call(context.Background(), "code", argsJSON, mcphub.CallMeta{})
}

func TestCodeBasicArithmetic(t *testing.T) {
	// Lua 5.3+ separates integers from floats: 2^10 is a float, so it prints
	// with the trailing ".0" (Lua 5.2 printed "1024").
	out, isErr, err := callCode(t, `{"code":"print(1+2*3)\nprint(2^10)\nprint(7//2)"}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if out != "7\n1024.0\n3\n" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestCodePrintCaptureFormatting(t *testing.T) {
	// Multiple args are tab-separated, each print ends with a newline,
	// and booleans/nil/floats stringify like standard Lua print.
	out, isErr, err := callCode(t, `{"code":"print(\"hi\", true, nil, 1.5, {1,2})"}`)
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

func TestCodeAllowedLibraries(t *testing.T) {
	out, isErr, err := callCode(t, `{
		"code": "print(string.upper('abc'), table.concat({1,2,3},'-'), math.floor(3.7), bit32.bxor(5,3), type(pcall))"
	}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if out != "ABC\t1-2-3\t3\t6\tfunction\n" {
		t.Fatalf("unexpected output: %q", out)
	}
}

// TestCodeLibrariesShopifyLacked covers the standard-Lua facilities the
// previous VM did not implement at all: pattern matching, binary packing,
// UTF-8, table.move.
func TestCodeLibrariesShopifyLacked(t *testing.T) {
	out, isErr, err := callCode(t, `{
		"code": "print(string.match('a=1','(%w+)=(%d+)'), (string.gsub('xxx','x','y')), utf8.len('héllo'), table.move({1},1,1,2,{9})[2], #string.pack('>i4',7), math.type(3.0))"
	}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if out != "a\tyyy\t5\t1\t4\tfloat\n" {
		t.Fatalf("unexpected output: %q", out)
	}
}

// TestCodeCoroutinesUnreachable covers why coroutines are removed: each
// one runs on its own goroutine, and an abandoned suspended coroutine costs
// ~16 KB that neither the deadline nor the checkpoint budget bounds, so within
// the budget a snippet can strand ~1.2M of them (~20 GB) and get the process
// OOM-killed. Nil-ing the global alone is not enough — package.loaded hands
// the module straight back through require().
func TestCodeCoroutinesUnreachable(t *testing.T) {
	before := runtime.NumGoroutine()
	wantOut(t, `print(type(coroutine), type(package.loaded.coroutine))
		local ok, m = pcall(require, 'coroutine')
		print(ok, type(m))`,
		"nil\tnil\nfalse\tstring\n")
	if n := runtime.NumGoroutine(); n > before+5 {
		t.Fatalf("a run spawned %d goroutines", n-before)
	}
}

// TestCodeSandboxBlocksHostLibraries checks the capability gating: no
// provider is set, so every host-facing module is never registered and
// dofile/loadfile do not exist.
func TestCodeSandboxBlocksHostLibraries(t *testing.T) {
	out, isErr, err := callCode(t, `{
		"code": "print(type(io), type(os), type(debug), type(chan), type(time), type(exec), type(http), type(dofile), type(loadfile))"
	}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if out != strings.Repeat("nil\t", 8)+"nil\n" {
		t.Fatalf("sandbox leak — expected all nil, got: %q", out)
	}
}

// TestCodeRemovedGlobals checks the globals we drop on top of the
// capability gating (see removedGlobals for why each one goes).
func TestCodeRemovedGlobals(t *testing.T) {
	out, isErr, err := callCode(t, `{
		"code": "print(type(collectgarbage), type(warn), type(glob), type(_lastoutput), type(_outputlines))"
	}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if out != strings.Repeat("nil\t", 4)+"nil\n" {
		t.Fatalf("unexpected globals: %q", out)
	}
}

// TestCodePackageIsInert proves require/searchpath/loadlib cannot reach
// the filesystem or dlopen anything without a code/loadlib provider.
func TestCodePackageIsInert(t *testing.T) {
	// require raises "module not found"; loadlib RETURNS the standard absent
	// triple (nil, errmsg, "absent") instead of raising; searchpath never
	// finds a real file because the searchers only reach disk via a provider.
	out, isErr, err := callCode(t, `{
		"code": "local ok = pcall(require,'os'); local f,msg,where = package.loadlib('/usr/lib/libc.so.6','printf'); print(ok, type(f), where, msg ~= nil, (package.searchpath('passwd','/etc/?')))"
	}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if out != "false\tnil\tabsent\ttrue\tnil\n" {
		t.Fatalf("package is not inert: %q", out)
	}
}

func TestCodeLoadIsTextOnly(t *testing.T) {
	// A valid text chunk still compiles and runs via load.
	out, isErr, err := callCode(t, `{"code":"local f = load('return 6*7'); print(f())"}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if out != "42\n" {
		t.Fatalf("text load broken: %q", out)
	}
}

func TestCodeLoadRejectsBinary(t *testing.T) {
	// Neither bytecode this VM produced (string.dump) nor hand-crafted bytes
	// carrying the Lua signature may reach the undumper.
	out, isErr, err := callCode(t, `{"code":"local f,e1 = load(string.dump(load('return 1'))); local g,e2 = load(string.char(27)..'Lua'..'GARBAGE'); print(type(f), e1 ~= nil, type(g), e2 ~= nil)"}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if out != "nil\ttrue\tnil\ttrue\n" {
		t.Fatalf("expected binary rejection, got: %q", out)
	}
}

func TestCodeLoadAcceptsReaderFunction(t *testing.T) {
	// Apart from the mode, stock load() semantics are preserved: the
	// reader-function form works (golua caps readers at 256 MB / 4M calls).
	out, isErr, err := callCode(t, `{"code":"local c={'return ','6*7'}; local i=0; local f=load(function() i=i+1; return c[i] end); print(f())"}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if out != "42\n" {
		t.Fatalf("reader load broken: %q", out)
	}
}

func TestCodeRecursiveMetamethodSafe(t *testing.T) {
	// A self-referential __tostring hits the VM's call-depth cap and must
	// surface as an in-band error, not a host crash.
	out, isErr, err := callCode(t, `{"code":"local t={}; setmetatable(t,{__tostring=function() return tostring(t) end}); print(tostring(t))"}`)
	if err != nil {
		t.Fatalf("expected in-band error, got Go error: %v", err)
	}
	if !isErr {
		t.Fatalf("expected isError=true for recursive metamethod, out=%q", out)
	}
}

func TestCodeDeepRecursionIsCatchable(t *testing.T) {
	// Limits.MaxCallDepth turns unbounded recursion into a Lua error the
	// snippet could even pcall — no Go stack exhaustion.
	out, isErr, err := callCode(t, `{"code":"local function f(n) return 1+f(n) end; print(f(0))"}`)
	if err != nil || !isErr {
		t.Fatalf("expected in-band error: out=%q isErr=%v err=%v", out, isErr, err)
	}
	if !strings.Contains(out, "stack overflow") {
		t.Fatalf("expected a stack overflow error, got: %q", out)
	}
}

func TestCodeRuntimeErrorSurfacesInBand(t *testing.T) {
	out, isErr, err := callCode(t, `{"code":"local t=nil; print(t.x)"}`)
	// Handler error -> registry returns it in-band with isError=true, no Go error.
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

func TestCodeSyntaxError(t *testing.T) {
	out, isErr, err := callCode(t, `{"code":"print(( "}`)
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

func TestCodeInfiniteLoopAborted(t *testing.T) {
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

// TestCodeRunawayPrintLoopBounded covers the reason the checkpoint
// budget exists at all: the capture buffer is an uncapped append, so a
// deadline alone would let a print loop retain hundreds of MB.
func TestCodeRunawayPrintLoopBounded(t *testing.T) {
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

func TestCodeCancelledContextAborts(t *testing.T) {
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

func TestCodeBoundedLoopUnderDefaultBudget(t *testing.T) {
	// A legit bounded loop must run fine under the production budget.
	out, err := runLua(context.Background(), "local s=0; for i=1,100000 do s=s+i end; print(s)")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out != "5000050000\n" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestCodeOutputTruncated(t *testing.T) {
	// 20k lines x 100 bytes = ~2 MB captured, well under the checkpoint
	// budget, so the run succeeds and the RESULT is what gets capped.
	out, isErr, err := callCode(t, `{"code":"for i=1,20000 do print(string.rep('x',100)) end"}`)
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

func TestCodeNoPrintProducesGuidance(t *testing.T) {
	out, isErr, err := callCode(t, `{"code":"local x=42"}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if !strings.Contains(out, "printed nothing") {
		t.Fatalf("expected no-output guidance, got: %q", out)
	}
}

func TestCodeArgumentValidation(t *testing.T) {
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
		out, isErr, err := callCode(t, tc.args)
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

// TestCodeDoesNotLeakToStdout proves print() is captured in-memory: the
// VM's fallback when nothing captures output is os.Stdout, so a missing
// WithCaptureOutput would show up as bytes on stdout here.
func TestCodeDoesNotLeakToStdout(t *testing.T) {
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

// TestCodeLua54Semantics pins the language the Description advertises.
// Every expectation here was probed against the running sandbox, and the ones
// that differ between golua v1 (Lua 5.4) and golua /v2 (Lua 5.5) are included
// deliberately: bumping the dependency — or a golua release that changes
// behaviour — fails this test instead of silently making the description lie
// to the model. The 5.5 branch makes for-loop control variables read-only,
// which is what killed a real snippet in production.
func TestCodeLua54Semantics(t *testing.T) {
	// Loop control variables are assignable in 5.4, a compile error in 5.5.
	wantOut(t, `for w in ('a b'):gmatch('%S+') do w = w:upper() print(w) end`, "A\nB\n")
	wantOut(t, `for i=1,3 do i = i*10 print(i) end`, "10\n20\n30\n")
	// table.create is 5.5-only; the compat aliases stock Lua dropped in 5.3
	// are still here in 5.4.
	wantOut(t, `print(table.create)`, "nil\n")
	wantOut(t, `print(type(math.pow), type(math.atan2), type(math.log10), type(math.cosh), type(math.frexp), type(math.ldexp), type(bit32))`,
		strings.Repeat("function\t", 6)+"table\n")
	wantOut(t, `print(math.pow(2,10), math.log10(1000), math.log(1000,10), bit32.bxor(5,3))`, "1024.0\t3.0\t3.0\t6\n")

	// Removed or renamed since 5.1.
	wantOut(t, `print(setfenv, getfenv, unpack, loadstring, table.maxn, table.getn, table.foreach, table.foreachi, module, newproxy, string.gfind, math.mod)`,
		strings.Repeat("nil\t", 11)+"nil\n")
	wantOut(t, `local a,b = table.unpack({1,2}) print(a, b, load('return 6*7')(), #'abc', select('#',1,nil,3), (table.pack(1,nil,3)).n, math.fmod(-7,3))`,
		"1\t2\t42\t3\t3\t3\t-1\n")
	// _ENV replaced setfenv/getfenv and is a plain assignable upvalue.
	wantOut(t, `print(type(_ENV), _ENV == _G)`, "table\ttrue\n")

	// Integers and floats (5.3).
	wantOut(t, `print(math.type(3), math.type(3.0), math.type('3'), tostring(3.0), 7/2, 7//2, 7.0//2, -7//2, 7%3, -7%3, math.fmod(-7,3))`,
		"integer\tfloat\tnil\t3.0\t3.5\t3\t3.0\t-4\t1\t2\t-1\n")
	wantOut(t, `print(math.maxinteger+1 == math.mininteger, math.tointeger(3.0), math.tointeger(3.5), math.type(math.floor(3.7)), math.type(math.ceil(3.2)), math.type(math.sqrt(9)), math.type(math.random(1,3)))`,
		"true\t3\tnil\tinteger\tinteger\tfloat\tinteger\n")
	// Floats print with 14 significant digits, which hides binary error.
	wantOut(t, `print(0.1+0.2, (0.1+0.2) == 0.3, string.format('%.17g', 0.1+0.2), 2^62, math.pi, tostring(2^10))`,
		"0.3\tfalse\t0.30000000000000004\t4.6116860184274e+18\t3.1415926535898\t1024.0\n")
	// Above 2^53 an integer does not survive a float round-trip.
	wantOut(t, `print(tostring(math.maxinteger), tostring(math.maxinteger + 0.0), math.maxinteger == math.maxinteger + 0.0)`,
		"9223372036854775807\t9.2233720368548e+18\tfalse\n")
	wantOut(t, `print(5&3, 5|3, 5~3, ~0, 1<<4, 16>>2, 5.0 & 3)`, "1\t7\t6\t-1\t16\t4\t1\n")
	// A float with a fractional part has no integer representation; 5.0 does.
	wantErr(t, `print(3.5 & 3)`, "number has no integer representation")
	wantErr(t, `print(string.format('%d', 3.5))`, "number has no integer representation")
	wantErr(t, `print(7//0)`, "attempt to divide by zero")
	wantOut(t, `local z=0 print(1/z, -1/z, 0/0)`, "inf\t-inf\t-nan\n")
	wantOut(t, `math.randomseed(42) local a=math.random(1,100) math.randomseed(42) print(a == math.random(1,100))`, "true\n")

	// New since 5.1.
	wantOut(t, `for i=1,3 do if i==2 then goto cont end print('i', i) ::cont:: end`, "i\t1\ni\t3\n")
	wantOut(t, `local log={} do local f <close> = setmetatable({}, {__close=function() log[#log+1]='closed' end}) end print(table.concat(log, ','))`,
		"closed\n")
	wantErr(t, `local c <const> = 7 c = 8 print(c)`, "attempt to assign to const variable")
	wantOut(t, `print(#('héllo'), utf8.len('héllo'), utf8.codepoint('é'), utf8.char(233))`, "6\t5\t233\té\n")
	wantOut(t, `print(#string.pack('>i4',7), string.unpack('>i4', string.pack('>i4',-7)), string.packsize('d'), string.packsize('>i4'))`,
		"4\t-7\t8\t4\n")
	wantErr(t, `print(string.packsize('z'))`, "variable-length format")
	wantOut(t, `print(string.rep('ab',3,'-'), rawlen('abc'), rawlen({1,2}), 0x1p4, 0x1p-1, table.move({1},1,1,2,{9})[2])`,
		"ab-ab-ab\t3\t2\t16.0\t0.5\t1\n")
	wantOut(t, `print(xpcall(function(a,b) return a+b end, function(e) return e end, 1, 2))`, "true\t3\n")
	// A numeric for at math.maxinteger terminates instead of wrapping (5.4).
	wantOut(t, `local n=0 for i=math.maxinteger-3, math.maxinteger do n=n+1 end print(n)`, "4\n")

	// Patterns, not regex.
	wantErr(t, `print(('a1'):find('\d'))`, "invalid escape sequence")
	wantOut(t, `print(('a1_!'):gsub('%a','#'), ('a1_!'):gsub('%w','#'), ('a1_!'):gsub('%p','#'))`, "#1_!\t##_!\ta1##\t2\n")
	wantOut(t, `print(('a.b'):find('.',1,true), ('a.b'):find('.'))`, "2\t1\t1\n")
	wantOut(t, `print(('2026-09-10'):match('(%d+)-(%d+)-(%d+)'))`, "2026\t09\t10\n")
	wantOut(t, `print(('<a><b>'):match('(.-)>'), ('hello world'):match('%f[%a]%w+%f[%A]'), ('a(b(c))d'):match('%b()'))`, "<a\thello\t(b(c))\n")
	wantOut(t, `print(('abc'):gsub('%w', {a='1'}), ('abc'):gsub('%w', function(c) return c:upper() end))`, "1bc\tABC\t3\n")
	wantOut(t, `local parts={} for p in ('a,b,,c'):gmatch('([^,]*)') do parts[#parts+1]=p end print(#parts, table.concat(parts,'|'))`,
		"4\ta|b||c\n")
	// A call truncates to one value except in tail position.
	wantOut(t, `print('x' .. ('aaa'):gsub('a','b'), ('aaa'):gsub('a','b'))`, "xbbb\tbbb\t3\n")

	// Tables.
	wantOut(t, `print(#{nil}, select('#',1,nil,3), (table.pack(1,nil,3)).n)`, "0\t3\t3\n")
	wantOut(t, `local n=0 for i,v in ipairs({1,2,nil,4}) do n=n+1 end print(n)`, "2\n")
	wantErr(t, `print(table.concat({1,nil,3}))`, "invalid value (nil) at index 2")
	wantErr(t, `local t={} for i=1,100 do t[i]=i end table.sort(t, function(a,b) return true end)`,
		"invalid order function for sorting")

	// No clock, no host: the description sends date work to time.
	wantOut(t, `print(os, io, debug, coroutine, dofile, loadfile, warn, collectgarbage)`,
		strings.Repeat("nil\t", 7)+"nil\n")
}
