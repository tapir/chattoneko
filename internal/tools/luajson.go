package tools

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/iceisfun/golua/v2/vm"
)

// A small "json" library for the simple_code sandbox: json.encode(value) and
// json.decode(text), both backed by the standard library's encoding/json.
//
// The conversion mirrors golua's own (unexported) JSON helpers in
// stdlib/http, so json.encode produces what the library itself would have
// sent as an HTTP body. Two things are added because a sandbox needs them:
//
//   - A nesting cap on encode. golua's converter recurses into tables with no
//     depth limit, so `local t={} t.self=t; json.encode(t)` exhausts the Go
//     stack. That is a runtime throw, not a panic — recover() cannot catch
//     it, the Registry's panic recovery is useless, and the whole server
//     process dies. It also happens inside a single native call, so neither
//     the checkpoint budget nor the context deadline can interrupt it.
//   - Exact integer decoding via json.Number. Decoding through float64 loses
//     precision above 2^53, which is exactly where int64 ids and timestamps
//     live.
//
// Errors raise as Lua errors (golua's native-function convention: panic with
// a message, the VM recovers it), so they are catchable with pcall and
// surface in-band through the tool registry.

// maxJSONDepth caps encode recursion. encoding/json rejects decode nesting
// past this same depth ("exceeded max depth"), so one constant bounds both
// directions and encode cannot produce what decode would refuse.
const maxJSONDepth = 10_000

// maxJSONBytes caps json.decode's input. Decoding is a single native call:
// no checkpoints fire inside it, so neither the deadline nor the checkpoint
// budget can bound it, and a snippet can hand us a string built by
// string.rep (up to golua's ~1 GB per-string cap) that would materialize
// several times its size in tables. Real input is JSON pasted into the
// snippet, which maxCodeBytes already bounds at 64 KB.
const maxJSONBytes = 16 * 1024 * 1024

// openJSON registers the json global. Called after stdlib.Open.
func openJSON(v *vm.VM) {
	lib := vm.NewEmptyTable()
	lib.SetString("encode", vm.NewNativeFunc(jsonEncode))
	lib.SetString("decode", vm.NewNativeFunc(jsonDecode))
	v.SetGlobal("json", vm.NewTable(lib))
}

// jsonEncode(value) -> string. Any value is accepted, not just tables, so
// json.encode(42) is "42". encoding/json escapes <, > and & as \u003c-style
// sequences; that is valid JSON and json.decode reverses it, so we leave the
// default rather than plumbing an Encoder with SetEscapeHTML(false).
func jsonEncode(v *vm.VM) int {
	data, err := json.Marshal(luaToGo(v.Get(1), 0))
	if err != nil {
		// NaN and ±Inf are the only values json.Marshal refuses. Its errors
		// are all "json: "-prefixed, which would read as "json.encode: json:".
		panic("json.encode: " + strings.TrimPrefix(err.Error(), "json: "))
	}
	v.Set(0, vm.NewString(string(data)))
	return 1
}

// jsonDecode(text) -> value.
func jsonDecode(v *vm.VM) int {
	arg := v.Get(1)
	if !arg.IsString() {
		panic(fmt.Sprintf("bad argument #1 to 'json.decode' (string expected, got %s)", arg.Type()))
	}
	text := arg.AsString()
	if len(text) > maxJSONBytes {
		panic(fmt.Sprintf("json.decode: input too large (%d bytes, max %d)", len(text), maxJSONBytes))
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber() // keep the literal text: int64 must not detour through float64
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		panic("json.decode: " + err.Error())
	}
	if dec.More() {
		// Decode reads one value; without this, `{"a":1} garbage` parses.
		panic("json.decode: unexpected data after the JSON value")
	}
	v.Set(0, goToLua(parsed))
	return 1
}

// luaToGo converts a Lua value into something json.Marshal can encode.
// Anything without a JSON form falls back to its string rendering, which is
// what golua's converter does — in practice only functions, since coroutines
// are removed from the sandbox — note that this puts a heap address in the
// output ("function: 0x34…").
func luaToGo(val vm.Value, depth int) any {
	if depth > maxJSONDepth {
		panic(fmt.Sprintf("json.encode: exceeded max nesting depth (%d)", maxJSONDepth))
	}
	switch {
	case val.IsNil():
		return nil
	case val.IsBool():
		return val.AsBool()
	case val.IsInt():
		return val.AsInt()
	case val.IsFloat():
		return val.AsFloat()
	case val.IsString():
		return val.AsString()
	case val.IsTable():
		return tableToGo(val.AsTable(), depth)
	default:
		return val.String()
	}
}

// tableToGo renders a table as a JSON array when its keys are exactly 1..n
// and as an object otherwise — including the empty table, which has no
// sequence and so becomes {} rather than [].
func tableToGo(tbl vm.LuaTable, depth int) any {
	if n := tbl.Len(); n > 0 && isSequence(tbl, n) {
		arr := make([]any, n)
		for i := 1; i <= n; i++ {
			arr[i-1] = luaToGo(tbl.Get(vm.NewInt(int64(i))), depth+1)
		}
		return arr
	}
	obj := make(map[string]any)
	key := vm.Nil
	for {
		next, val, err := tbl.Next(key)
		if err != nil || next.IsNil() {
			return obj
		}
		obj[luaKeyString(next)] = luaToGo(val, depth+1)
		key = next
	}
}

// isSequence reports whether every slot in 1..length is filled and the table
// holds no key outside that range. Len() reports the sequence part only (a
// lone t[1e9] gives 0), so both scans are bounded by the real array length.
func isSequence(tbl vm.LuaTable, length int) bool {
	for i := 1; i <= length; i++ {
		if tbl.Get(vm.NewInt(int64(i))).IsNil() {
			return false
		}
	}
	key := vm.Nil
	for {
		next, _, err := tbl.Next(key)
		if err != nil || next.IsNil() {
			return true
		}
		if !next.IsInt() || next.AsInt() < 1 || next.AsInt() > int64(length) {
			return false
		}
		key = next
	}
}

func luaKeyString(key vm.Value) string {
	switch {
	case key.IsString():
		return key.AsString()
	case key.IsInt():
		return strconv.FormatInt(key.AsInt(), 10)
	case key.IsFloat():
		return strconv.FormatFloat(key.AsFloat(), 'g', -1, 64)
	default:
		return key.String()
	}
}

// goToLua converts a decoded JSON value into a Lua value. JSON null is not
// modelled: it becomes nil, and since assigning nil removes a table key, a
// null member simply disappears ({"a":null} -> {}) and a null array element
// leaves a hole ([1,null,3] -> t[2] == nil). That is what dkjson and golua's
// own converter do. Nesting needs no depth guard here: encoding/json stops at
// maxJSONDepth before this is ever called.
func goToLua(val any) vm.Value {
	switch v := val.(type) {
	case nil:
		return vm.Nil
	case bool:
		return vm.NewBool(v)
	case string:
		return vm.NewString(v)
	case json.Number:
		return jsonNumberToLua(v)
	case []any:
		tbl := vm.NewEmptyTable()
		for i, elem := range v {
			tbl.Set(vm.NewInt(int64(i+1)), goToLua(elem))
		}
		return vm.NewTable(tbl)
	case map[string]any:
		tbl := vm.NewEmptyTable()
		for key, elem := range v {
			tbl.SetString(key, goToLua(elem))
		}
		return vm.NewTable(tbl)
	default:
		return vm.NewString(fmt.Sprintf("%v", v))
	}
}

// jsonNumberToLua keeps integers exact: UseNumber hands us the literal text,
// so an int64 that float64 could not represent survives the round trip.
// A number too large for either (1e400) is rejected rather than silently
// becoming ±Inf.
func jsonNumberToLua(n json.Number) vm.Value {
	if i, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
		return vm.NewInt(i)
	}
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil {
		panic("json.decode: number out of range: " + n.String())
	}
	return vm.NewFloat(f)
}
