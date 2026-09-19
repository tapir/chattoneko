package tools

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"chattoneko/internal/mcphub"
)

// requireJQ skips when the host has no jq, the same way api_test skips without
// ffmpeg. The image always ships one; a dev machine may not.
func requireJQ(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(Binary()); err != nil {
		t.Skipf("no %s on PATH", Binary())
	}
}

// callJQ invokes the registered tool through the catalog the same way the
// engine does, returning (result, isError, err).
func callJQ(t *testing.T, argsJSON string) (string, bool, error) {
	t.Helper()
	requireJQ(t)
	return Builtin(nil, nil).Call(context.Background(), "jq", argsJSON, mcphub.CallMeta{})
}

// wantJQ asserts a call succeeded and produced exactly out.
func wantJQ(t *testing.T, argsJSON, out string) {
	t.Helper()
	got, isErr, err := callJQ(t, argsJSON)
	if err != nil || isErr {
		t.Fatalf("args=%s: out=%q isErr=%v err=%v", argsJSON, got, isErr, err)
	}
	if got != out {
		t.Fatalf("args=%s:\n got %q\nwant %q", argsJSON, got, out)
	}
}

func TestJQArithmeticWithoutInput(t *testing.T) {
	// No input means -n: "." is null and jq never reads stdin, so pure
	// arithmetic is a one-argument call.
	wantJQ(t, `{"filter":"pow(2;10)"}`, "1024\n")
	wantJQ(t, `{"filter":"1+2*3"}`, "7\n")
	wantJQ(t, `{"filter":"7/2"}`, "3.5\n")
}

func TestJQInputOnStdin(t *testing.T) {
	wantJQ(t, `{"filter":".a|map(.*2)","input":{"a":[1,2,3]}}`, "[2,4,6]\n")
	// The input is a JSON value, not a string holding JSON: a bare scalar and a
	// nested structure both arrive verbatim.
	wantJQ(t, `{"filter":".+.","input":21}`, "42\n")
	wantJQ(t, `{"filter":".[]|.n","input":[{"n":1},{"n":2}]}`, "1\n2\n")
	// An explicit null is input, not its absence.
	wantJQ(t, `{"filter":". == null","input":null}`, "true\n")
}

func TestJQBigNumberSemantics(t *testing.T) {
	// decNumber keeps a literal's digits, so an id survives being read and
	// printed; arithmetic is IEEE754 double regardless. Both halves are pinned
	// because the tool description promises exactly this split.
	wantJQ(t, `{"filter":".","input":12345678901234567890}`, "12345678901234567890\n")
	wantJQ(t, `{"filter":"tostring","input":12345678901234567890}`, `"12345678901234567890"`+"\n")
	wantJQ(t, `{"filter":". * 2","input":12345678901234567890}`, "24691357802469134000\n")
	// Exact up to 2^53, and the next integer after it does not exist.
	wantJQ(t, `{"filter":". + 1","input":9007199254740991}`, "9007199254740992\n")
	wantJQ(t, `{"filter":". + 1","input":9007199254740992}`, "9007199254740992\n")
	wantJQ(t, `{"filter":"pow(2;53) + 1"}`, "9007199254740992\n")
}

func TestJQRegex(t *testing.T) {
	// oniguruma is built in, so these are real regular expressions.
	wantJQ(t, `{"filter":"test(\"w.rld\")","input":"hello world"}`, "true\n")
	wantJQ(t, `{"filter":"capture(\"(?<w>\\\\w+)\")","input":"hi there"}`, `{"w":"hi"}`+"\n")
	wantJQ(t, `{"filter":"[splits(\",\")]","input":"a,b,c"}`, `["a","b","c"]`+"\n")
}

func TestJQEnvironmentIsEmpty(t *testing.T) {
	requireJQ(t)
	// The server's environment holds the provider key and the login password.
	// jq reaches it through $ENV and env unless the child gets nothing.
	t.Setenv("CHATTO_JQ_LEAK_PROBE", "secret")
	wantJQ(t, `{"filter":"$ENV"}`, "{}\n")
	wantJQ(t, `{"filter":"env|length"}`, "0\n")
	wantJQ(t, `{"filter":"env.CHATTO_JQ_LEAK_PROBE"}`, "null\n")
}

func TestJQGetsNoFileArguments(t *testing.T) {
	// The filter is the only non-flag argument, so jq has no path to open and
	// nothing to report a filename for.
	wantJQ(t, `{"filter":"input_filename"}`, "null\n")
	// stdin carries exactly the one value the model sent, and nothing else.
	wantJQ(t, `{"filter":"[inputs]|length","input":{"a":1}}`, "0\n")
}

func TestJQErrorsComeBackInBand(t *testing.T) {
	requireJQ(t)
	for _, tc := range []struct{ name, args, want string }{
		{"compile error", `{"filter":"syntax(("}`, "syntax error"},
		{"runtime error", `{"filter":"error(\"boom\")"}`, "boom"},
		{"type error", `{"filter":".a","input":3}`, "Cannot index number"},
		{"unknown function", `{"filter":"nosuchfn(1)"}`, "nosuchfn/1 is not defined"},
		{"halt_error", `{"filter":"halt_error"}`, "jq did not finish normally"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, isErr, err := callJQ(t, tc.args)
			if err != nil {
				t.Fatalf("host error: %v", err)
			}
			if !isErr {
				t.Fatalf("expected an in-band error, got %q", out)
			}
			if !strings.HasPrefix(out, "Error: ") {
				t.Fatalf("result is not the registry's in-band form: %q", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("result %q does not mention %q", out, tc.want)
			}
		})
	}
}

func TestJQOutputTruncatedAndKilled(t *testing.T) {
	requireJQ(t)
	// range streams, so this hits the cap without jq building anything large
	// first — the point is that the read stops and the process dies at the cap
	// instead of buffering 14 MB or waiting out the 30s deadline.
	start := time.Now()
	out, isErr, err := callJQ(t, `{"filter":"range(2000000)"}`)
	if err != nil || isErr {
		t.Fatalf("out=%d bytes isErr=%v err=%v", len(out), isErr, err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("runaway output took %v — the cap did not kill jq", elapsed)
	}
	if len(out) > maxOutputBytes+128 {
		t.Fatalf("result is %d bytes, cap is %d", len(out), maxOutputBytes)
	}
	if !strings.HasPrefix(out, "0\n1\n2\n") {
		t.Fatalf("unexpected prefix: %.40q", out)
	}
	if !strings.Contains(out, "output truncated") || !strings.Contains(out, "narrow the filter") {
		t.Fatalf("truncation is not announced: %.80q", out[len(out)-80:])
	}
}

func TestJQStderrIsCapped(t *testing.T) {
	requireJQ(t)
	// One error per input value: the diagnostics must be bounded while the
	// pipe is still drained, or jq blocks on a full pipe and the call waits
	// out its deadline.
	start := time.Now()
	out, isErr, err := callJQ(t, `{"filter":".[]|error(\"x\")","input":[1,2,3,4,5,6,7,8,9,10]}`)
	if err != nil {
		t.Fatalf("host error: %v", err)
	}
	if !isErr {
		t.Fatalf("expected an in-band error, got %q", out)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("erroring filter took %v", elapsed)
	}
	if len(out) > maxJQStderrBytes+64 {
		t.Fatalf("stderr was not capped: %d bytes", len(out))
	}
}

func TestJQCancelledContextAborts(t *testing.T) {
	requireJQ(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := runJQ(ctx, `{"filter":"range(100000000)"}`, mcphub.CallMeta{})
	if err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("cancelled run took %v", elapsed)
	}
}

func TestJQMissingBinaryIsInBand(t *testing.T) {
	old := jqBin
	jqBin = "/nonexistent/jq"
	defer func() { jqBin = old }()

	out, isErr, err := Builtin(nil, nil).Call(context.Background(), "jq", `{"filter":"1"}`, mcphub.CallMeta{})
	if err != nil {
		t.Fatalf("host error: %v", err)
	}
	if !isErr || !strings.Contains(out, "cannot run") {
		t.Fatalf("out=%q isErr=%v", out, isErr)
	}
}

func TestJQArgumentValidation(t *testing.T) {
	requireJQ(t)
	for _, tc := range []struct{ name, args, want string }{
		{"no arguments", ``, "invalid arguments JSON"},
		{"empty filter", `{"filter":"   "}`, "filter is required"},
		{"oversized filter", `{"filter":"` + strings.Repeat("x", maxJQFilterBytes+1) + `"}`, "filter too large"},
		{"oversized input", `{"filter":".","input":"` + strings.Repeat("x", maxJQInputBytes+1) + `"}`, "input too large"},
		{"malformed arguments", `{"filter":`, "invalid arguments JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, isErr, err := callJQ(t, tc.args)
			if err != nil {
				t.Fatalf("host error: %v", err)
			}
			if !isErr {
				t.Fatalf("expected an in-band error, got %q", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("result %q does not mention %q", out, tc.want)
			}
		})
	}
}

func TestJQEmptyOutputIsExplained(t *testing.T) {
	// A filter that yields nothing is not an error, and silence would leave the
	// model guessing whether the call ran at all.
	out, isErr, err := callJQ(t, `{"filter":"empty"}`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if !strings.Contains(out, "produced no output") {
		t.Fatalf("unexpected result: %q", out)
	}
	// Same for a select that keeps nothing.
	out, isErr, err = callJQ(t, `{"filter":".[]|select(. > 5)","input":[1,2]}`)
	if err != nil || isErr || !strings.Contains(out, "produced no output") {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
}

func TestJQFilterBeginningWithADash(t *testing.T) {
	// "--" separates the flags from the filter, so a leading dash is a filter
	// and not an option jq would reject.
	wantJQ(t, `{"filter":"-1"}`, "-1\n")
	wantJQ(t, `{"filter":"-.5|tostring"}`, `"-0.5"`+"\n")
}
