package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"chattoneko/internal/mcphub"
)

// The "jq" tool: runs a jq filter over JSON the model supplies and returns what
// the filter produces — exact arithmetic, string matching and data wrangling
// instead of guessing, for work already shaped like JSON, where a filter is
// shorter than a program and the model needs no syntax tutorial because jq is
// jq.
//
// Sandboxing is capability-based and rests on how jq is invoked rather than on
// inspecting the filter:
//
//   - no file arguments, ever: the filter is the only non-flag argument, so
//     there is no path for jq to open, and -- keeps a filter that begins with a
//     dash from being read as an option.
//   - cmd.Env is empty, not inherited: jq exposes the process environment
//     through $ENV and env, which would otherwise hand the model
//     CHATTO_PASSWORD and every provider key the server was started with.
//
// Text scanning for "$ENV" would be the wrong shape: trivially bypassed
// (env|to_entries) and it would reject legitimate filters that merely mention
// the word in a string.
//
// Bounds: the filter and the input are capped on the way in, the result at
// maxOutputBytes on the way out, and the wall clock by the handler's context
// (registry.Call gives every integrated tool 30s, and exec.CommandContext kills
// the process when it lands). Output is read with CopyN rather than collected,
// so a filter that produces gigabytes is killed at the cap instead of being
// buffered first.
//
// jq's own memory is not bounded: a filter like [range(1e8)] allocates the whole
// array before printing anything.
// ponytail: jq has no memory flag and a subprocess OOM kill is survivable — the
// kernel picks the largest process, which is jq, and the tool reports the kill
// in-band. Set RLIMIT_AS through SysProcAttr (a _linux.go pair like
// droproot_linux.go) if that ever proves too optimistic.
//
// Every failure path returns a Go error, which registry.Call surfaces in-band
// as "Error: ..." — a missing jq binary, a compile error, a runtime error,
// malformed arguments, a timeout and an OOM kill all reach the model as tool
// output, and none of them can reach the host. Malformed INPUT is not one of
// them: it arrives as json.RawMessage, so a value that is not JSON never gets
// past the arguments parse.

// EnvJQBinary overrides the jq to run, mirroring media.EnvBinary: the image
// ships the static build jq/build.sh produces at /usr/local/bin/jq, a dev
// machine uses its own.
const EnvJQBinary = "CHATTO_JQ"

var jqBin = func() string {
	if p := strings.TrimSpace(os.Getenv(EnvJQBinary)); p != "" {
		return p
	}
	return "jq"
}()

// Binary is the jq this tool runs: $CHATTO_JQ when set, otherwise a plain "jq"
// PATH lookup. Read once, like every other CHATTO_ variable.
func Binary() string { return jqBin }

// maxJQFilterBytes bounds the filter. A jq program is a few lines; this only
// keeps a pathological payload from reaching the compiler.
const maxJQFilterBytes = 64 * 1024

// maxJQInputBytes bounds the JSON fed to stdin. The model has to type the input
// into its own tool call, so anything near this is already a paste; the cap
// keeps one call from putting an arbitrary amount of data through jq's parser.
const maxJQInputBytes = 1 << 20 // 1 MiB

// maxJQStderrBytes bounds what jq's error stream can retain. A filter that
// raises once per input value would otherwise fill memory with diagnostics; the
// first few lines are all the model can act on anyway.
const maxJQStderrBytes = 16 * 1024

var jqSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"filter": {
			"type": "string",
			"description": "A jq program run against the input. Example: .items[] | select(.n > 3) | .name"
		},
		"input": {
			"description": "The JSON value the filter runs on, given as JSON (not as a string holding JSON). Omit it for no input, which is what pure arithmetic and string work want."
		}
	},
	"required": ["filter"],
	"additionalProperties": false
}`)

var JQ = tool{
	Name: "jq",
	Description: `Run a jq 1.8 filter. The filter is a jq program; input is the JSON value it runs against, given as JSON and fed to jq on stdin as one value. Omit input for no input at all — arithmetic and string work need none.

Output is compact JSON, one line per value the filter produces. A bad filter or a runtime error comes back as the tool result, so a mistake costs one call.

Nothing outside that value is reachable: no files, no command-line arguments, no network, no input_filename, and the environment is empty — $ENV and env both yield {}. Everything jq can do to a value in memory, it can do here.

Reach for it for arithmetic you need right (pow(2;10), not 2^10 — there is no ^ operator), real regular expressions (test, match, capture, scan, sub, gsub, splits are oniguruma, so \d and (?<name>…) work), and for querying, reshaping, sorting, grouping, joining or counting JSON. Also base64, URL escaping, date math from a unix timestamp, and string splitting/padding/case work.

Emit a summary, not a dump — the result is capped and a truncated one is a wasted call.`,
	Schema:         jqSchema,
	DefaultEnabled: true,
	Title:          "Running jq…",
	Handler:        runJQ,
}

func runJQ(ctx context.Context, argsJSON string, _ mcphub.CallMeta) (string, error) {
	var args struct {
		Filter string          `json:"filter"`
		Input  json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments JSON: %v", err)
	}
	filter := strings.TrimSpace(args.Filter)
	if filter == "" {
		return "", errors.New("filter is required")
	}
	if len(filter) > maxJQFilterBytes {
		return "", fmt.Errorf("filter too large (max %d bytes)", maxJQFilterBytes)
	}
	if len(args.Input) > maxJQInputBytes {
		return "", fmt.Errorf("input too large (max %d bytes)", maxJQInputBytes)
	}

	// -n only when the model sent no input: jq then never reads stdin and "."
	// is null, which is what a filter doing arithmetic wants. An explicit JSON
	// null arrives as input and goes through stdin like any other value.
	flags := []string{"-c"}
	if len(args.Input) == 0 {
		flags = append(flags, "-n")
	}
	cmd := exec.CommandContext(ctx, jqBin, append(flags, "--", filter)...)
	cmd.Env = []string{} // no $ENV, so no CHATTO_PASSWORD or provider key

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	stderr := cappedBuffer{limit: maxJQStderrBytes}
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("cannot run %s: %v", jqBin, err)
	}
	// Written in a goroutine and its errors dropped: jq exits without reading
	// stdin on a compile error, and that EPIPE must not mask the real message.
	go func() {
		_, _ = stdin.Write(args.Input)
		_ = stdin.Close()
	}()

	var out bytes.Buffer
	n, _ := io.CopyN(&out, stdout, maxOutputBytes+1)
	truncated := n > maxOutputBytes
	if truncated {
		_ = cmd.Process.Kill() // the cap is reached; there is nothing left to read
	}
	waitErr := cmd.Wait()

	switch {
	// Truncation kills jq on purpose, so its exit status reports the kill and
	// nothing else: the cap was reached and what was read is the result.
	case truncated:
		return jqResult(out.String(), true), nil
	case ctx.Err() != nil:
		return "", fmt.Errorf("jq was aborted before finishing: %v", ctx.Err())
	case waitErr != nil:
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) {
			return "", waitErr
		}
		if msg := strings.TrimSpace(stderr.buf.String()); msg != "" {
			return "", errors.New(msg)
		}
		return "", fmt.Errorf("jq did not finish normally (%v) and reported nothing", exit)
	}
	return jqResult(out.String(), false), nil
}

// jqResult renders what jq wrote. The cap is enforced here rather than by
// reading everything first: CopyN stopped at maxOutputBytes+1 and the process
// was killed, so out holds at most one byte more than the budget.
func jqResult(out string, truncated bool) string {
	if truncated {
		// ponytail: the cut can split a multi-byte rune; encoding/json replaces
		// the stray bytes with U+FFFD on the way to the model.
		return out[:maxOutputBytes] +
			fmt.Sprintf("\n(output truncated at %d bytes — narrow the filter)\n", maxOutputBytes)
	}
	if out == "" {
		return "(the filter produced no output — `empty`, a `select` that kept nothing and an `if` with no matching branch all yield nothing)"
	}
	return out
}

// cappedBuffer retains the first limit bytes and discards the rest while still
// reporting every byte as written. os/exec drains cmd.Stderr through a pipe and
// a goroutine, so discarding rather than refusing keeps jq from blocking on a
// full pipe while still bounding what a chatty filter can retain.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	// len(p) BEFORE truncating: reporting fewer bytes than handed over makes
	// os/exec's io.Copy fail with ErrShortWrite and a clean jq run an error.
	n := len(p)
	if room := c.limit - c.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		c.buf.Write(p)
	}
	return n, nil
}
