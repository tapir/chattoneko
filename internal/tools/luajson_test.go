package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"chattoneko/internal/mcphub"
)

// callLua runs a snippet through the tool the way the engine does, without
// hand-escaping the code into JSON.
func callLua(t *testing.T, code string) (string, bool, error) {
	t.Helper()
	args, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		t.Fatal(err)
	}
	return Builtin(nil, nil).Call(context.Background(), "code", string(args), mcphub.CallMeta{})
}

// wantOut runs code and asserts the exact printed output.
func wantOut(t *testing.T, code, want string) {
	t.Helper()
	out, isErr, err := callLua(t, code)
	if err != nil || isErr {
		t.Fatalf("%s\n  out=%q isErr=%v err=%v", code, out, isErr, err)
	}
	if out != want {
		t.Fatalf("%s\n  got  %q\n  want %q", code, out, want)
	}
}

// wantErr runs code and asserts it fails in-band (isError=true, no Go error)
// with a message containing want.
func wantErr(t *testing.T, code, want string) {
	t.Helper()
	out, isErr, err := callLua(t, code)
	if err != nil {
		t.Fatalf("%s\n  expected an in-band error, got a Go error: %v", code, err)
	}
	if !isErr {
		t.Fatalf("%s\n  expected isError=true, got out=%q", code, out)
	}
	if !strings.Contains(out, want) {
		t.Fatalf("%s\n  want %q in %q", code, want, out)
	}
}

func TestJSONEncodeScalars(t *testing.T) {
	wantOut(t, `print(json.encode(42), json.encode(1.5), json.encode('x'), json.encode(true), json.encode(nil))`,
		"42\t1.5\t\"x\"\ttrue\tnull\n")
}

func TestJSONEncodeArrayVsObject(t *testing.T) {
	wantOut(t, `print(json.encode({1,2,3}))`, "[1,2,3]\n")
	wantOut(t, `print(json.encode({}))`, "{}\n") // no sequence -> object
	wantOut(t, `local t={1,2}; t.x=3; print(json.encode(t))`, `{"1":1,"2":2,"x":3}`+"\n")
	wantOut(t, `local t={1,2,nil,4}; print(json.encode(t))`, `{"1":1,"2":2,"4":4}`+"\n")
	// Object keys come out sorted: encoding/json marshals maps in key order.
	wantOut(t, `print(json.encode({b=1,a=2}))`, `{"a":2,"b":1}`+"\n")
}

func TestJSONDecodeStructure(t *testing.T) {
	wantOut(t, `local t=json.decode('{"name":"alice","tags":["a","b"],"n":42,"f":1.5,"ok":true}')
		print(t.name, t.tags[1], t.tags[2], t.n, t.f, t.ok)`,
		"alice\ta\tb\t42\t1.5\ttrue\n")
	wantOut(t, `print(json.decode('"just a string"'), json.decode('null'), json.decode('[1,2]')[2])`,
		"just a string\tnil\t2\n")
}

func TestJSONNumberTypes(t *testing.T) {
	// A whole number stays an integer, a fractional or exponent literal is a
	// float — Lua 5.3+ keeps the distinction, and so does math.type.
	wantOut(t, `print(math.type(json.decode('123')), math.type(json.decode('1.5')), math.type(json.decode('2.0')), math.type(json.decode('1e3')))`,
		"integer\tfloat\tfloat\tfloat\n")
}

func TestJSONBigintStaysExact(t *testing.T) {
	// Above 2^53 a float64 detour would silently corrupt these; UseNumber
	// hands us the literal text, so ParseInt keeps them exact.
	wantOut(t, `print(json.decode('9007199254740993'), json.decode('-9223372036854775808'))`,
		"9007199254740993\t-9223372036854775808\n")
	wantErr(t, `print(json.decode('1e400'))`, "number out of range")
}

func TestJSONUnicode(t *testing.T) {
	wantOut(t, `print(json.encode({s='héllo 🙂'}))`, `{"s":"héllo 🙂"}`+"\n")
	wantOut(t, `print(json.decode('"\\u00e9\\ud83d\\ude42"'))`, "é🙂\n")
}

func TestJSONNullBecomesNil(t *testing.T) {
	// Assigning nil removes a table key, so a null member disappears and a
	// null array element leaves a hole. Documented behaviour, matches dkjson.
	wantOut(t, `local t=json.decode('{"a":null,"b":1}'); print(t.a, t.b)`, "nil\t1\n")
	wantOut(t, `local a=json.decode('[1,null,3]'); print(a[1], a[2], a[3])`, "1\tnil\t3\n")
}

func TestJSONRoundTrip(t *testing.T) {
	wantOut(t, `local orig={id=9007199254740993, list={1,2,3}, nested={k='v'}}
		local back=json.decode(json.encode(orig))
		print(back.id, back.list[2], back.nested.k, math.type(back.id))`,
		"9007199254740993\t2\tv\tinteger\n")
}

func TestJSONDecodeErrors(t *testing.T) {
	wantErr(t, `print(json.decode('{bad'))`, "json.decode")
	wantErr(t, `print(json.decode(''))`, "json.decode")
	// Decode reads a single value; trailing junk must not pass silently.
	wantErr(t, `print(json.decode('{"a":1} junk'))`, "unexpected data after the JSON value")
	wantErr(t, `print(json.decode(42))`, "bad argument #1 to 'json.decode' (string expected, got number)")
	// encoding/json's own nesting limit, hit before goToLua ever recurses.
	wantErr(t, `print(json.decode(string.rep('[',10001)..string.rep(']',10001)))`, "exceeded max depth")
}

func TestJSONDecodeErrorsAreCatchable(t *testing.T) {
	wantOut(t, `local ok,msg = pcall(json.decode, '{bad'); print(ok, type(msg))`, "false\tstring\n")
}

func TestJSONEncodeRejectsNaNAndInf(t *testing.T) {
	wantOut(t, `print(pcall(json.encode, 0/0))`, "false\tjson.encode: unsupported value: NaN\n")
	wantOut(t, `local ok,err = pcall(json.encode, math.huge); print(ok, err)`, "false\tjson.encode: unsupported value: +Inf\n")
}

// TestJSONEncodeCyclicTableIsAnError is the reason maxJSONDepth exists. With
// no depth guard this recursion exhausts the Go stack, which is a runtime
// throw that recover() cannot catch: it would kill the whole server process,
// and neither the checkpoint budget nor the context deadline can interrupt it
// because it happens inside a single native call.
func TestJSONEncodeCyclicTableIsAnError(t *testing.T) {
	wantErr(t, `local t={a=1}; t.self=t; print(json.encode(t))`, "exceeded max nesting depth")
	wantOut(t, `local t={}; t.self=t; local ok,err=pcall(json.encode,t); print(ok, type(err))`, "false\tstring\n")
	// Two tables pointing at each other, not just a self-reference.
	wantErr(t, `local a={}; local b={a=a}; a.b=b; print(json.encode(a))`, "exceeded max nesting depth")
}

func TestJSONEncodeDeepButLegal(t *testing.T) {
	wantOut(t, `local t={} local cur=t
		for i=1,50 do cur.nested={} cur=cur.nested end
		cur.leaf=1
		local back=json.decode(json.encode(t))
		local c=back for i=1,50 do c=c.nested end
		print(c.leaf)`, "1\n")
	wantErr(t, `local t={} local cur=t
		for i=1,20000 do cur.nested={} cur=cur.nested end
		print(json.encode(t))`, "exceeded max nesting depth")
}

func TestJSONEncodeNonEncodableFallsBackToString(t *testing.T) {
	// A function has no JSON form; like golua's own converter we render it as
	// its string form rather than raising.
	out, isErr, err := callLua(t, `print(json.encode({f=function() end}))`)
	if err != nil || isErr {
		t.Fatalf("out=%q isErr=%v err=%v", out, isErr, err)
	}
	if !strings.HasPrefix(out, `{"f":"function: 0x`) {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestJSONDecodeInputTooLarge(t *testing.T) {
	wantOut(t, `local ok,err = pcall(json.decode, string.rep('x', 16*1024*1024+1)); print(ok, err ~= nil)`,
		"false\ttrue\n")
}
