package tools

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"chattoneko/internal/mcphub"
)

// callSimpleCode invokes the registered tool through the catalog the same way
// the engine does, returning (result, isError, err).
func callSimpleCode(t *testing.T, argsJSON string) (string, bool, error) {
	t.Helper()
	return Builtin(nil, nil).Call(context.Background(), "simple_code", argsJSON, mcphub.CallMeta{})
}

func TestSimpleCodeBasicArithmetic(t *testing.T) {
	out, isErr, err := callSimpleCode(t, `{"code":"print(1+2*3)\nprint(2^10)"}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if out != "7\n1024\n" {
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
	// All five whitelisted libraries must be present and functional.
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

func TestSimpleCodeSandboxBlocksOtherLibraries(t *testing.T) {
	// io/os/package/debug/coroutine are not opened, so they are nil globals.
	out, isErr, err := callSimpleCode(t, `{
		"code": "print(type(io), type(os), type(package), type(debug), type(coroutine), type(require))"
	}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if out != "nil\tnil\tnil\tnil\tnil\tnil\n" {
		t.Fatalf("sandbox leak — expected all nil, got: %q", out)
	}
}

// TestSimpleCodeSandboxHardening verifies the base-library globals that
// would defeat the sandbox are neutralized, and that the text-only
// `load` replacement remains a function.
func TestSimpleCodeSandboxHardening(t *testing.T) {
	// dofile/loadfile (filesystem) and collectgarbage (host GC) are
	// removed; load is replaced but remains a function.
	out, isErr, err := callSimpleCode(t, `{"code":"print(type(dofile), type(loadfile), type(collectgarbage), type(load))"}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if out != "nil\tnil\tnil\tfunction\n" {
		t.Fatalf("unexpected hardened globals: %q", out)
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

func TestSimpleCodeLoadRejectsBinaryAndFunction(t *testing.T) {
	// Binary chunks (Lua signature prefix) must be rejected, and the
	// function-reader form of load is not supported.
	out, isErr, err := callSimpleCode(t, `{"code":"local f1,e1 = load(string.char(27)..'Lua'..'GARBAGE'); local f2,e2 = load(function() return nil end); print(type(f1), type(f2)); print(e1 and string.find(e1, 'binary') and 'binary-rejected' or 'no'); print(e2 and string.find(e2, 'literal') and 'fnreader-rejected' or 'no')"}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if !strings.Contains(out, "nil\tnil\n") || !strings.Contains(out, "binary-rejected") || !strings.Contains(out, "fnreader-rejected") {
		t.Fatalf("expected binary+function-reader rejection, got: %q", out)
	}
}

func TestSimpleCodeRecursiveMetamethodSafe(t *testing.T) {
	// A self-referential __tostring hits the library's nested-call cap and
	// must surface as an in-band error, not a host crash.
	out, isErr, err := callSimpleCode(t, `{"code":"local t={}; setmetatable(t,{__tostring=function() return tostring(t) end}); print(tostring(t))"}`)
	if err != nil {
		t.Fatalf("expected in-band error, got Go error: %v", err)
	}
	if !isErr {
		t.Fatalf("expected isError=true for recursive metamethod, out=%q", out)
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
	if !strings.Contains(out, "syntax error") {
		t.Fatalf("expected syntax error, got: %q", out)
	}
}

func TestSimpleCodeInfiniteLoopAborted(t *testing.T) {
	// A tiny budget aborts a `while true` loop almost immediately via the
	// count-hook, returning an error instead of hanging the host.
	_, err := runLua("while true do end", 5*luaHookBatch)
	if err == nil {
		t.Fatal("expected instruction-budget error for infinite loop")
	}
	if !strings.Contains(err.Error(), "instruction budget exceeded") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSimpleCodeBoundedLoopUnderDefaultBudget(t *testing.T) {
	// A legit bounded loop must run fine under the production budget.
	out, err := runLua("local s=0; for i=1,100000 do s=s+i end; print(s)", luaInstructionBudget)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out != "5000050000\n" {
		t.Fatalf("unexpected output: %q", out)
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

// TestSimpleCodeDoesNotLeakToStdout proves the print override is in effect:
// the default go-lua print writes to os.Stdout, so if the override were
// missing this test would observe bytes on stdout.
func TestSimpleCodeDoesNotLeakToStdout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	os.Stdout = w

	_, _ = runLua("print('leak-check')", luaInstructionBudget)

	_ = w.Close()
	os.Stdout = oldStdout
	captured, _ := io.ReadAll(r)
	_ = r.Close()

	if len(captured) != 0 {
		t.Fatalf("print() leaked to stdout: %q", captured)
	}
}
