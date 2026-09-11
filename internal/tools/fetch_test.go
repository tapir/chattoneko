package tools

import (
	"bytes"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"chattoneko/internal/mcphub"
	"chattoneko/internal/webfetch"
)

// ---- shared HTTP test helpers (also used by the create_file url source) ----

// allowWebFetchLoopback lets the fetch tests reach their httptest servers on
// 127.0.0.1, which the SSRF blocklist rejects by default.
func allowWebFetchLoopback(t *testing.T) {
	t.Helper()
	webfetch.AllowLoopbackForTesting(true)
	t.Cleanup(func() { webfetch.AllowLoopbackForTesting(false) })
}

// testJPEG returns a small JPEG image's bytes.
func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

// serve returns a server answering every path with body (and contentType when
// given), closed with the test.
func serve(t *testing.T, contentType string, body []byte) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.Write(body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// servePaths answers each listed path with body and its own Content-Type; any
// other path gets body with no type.
func servePaths(t *testing.T, types map[string]string, body []byte) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct, ok := types[r.URL.Path]; ok {
			w.Header().Set("Content-Type", ct)
		}
		w.Write(body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// serveTextOrImage answers .md paths with text and everything else with jpg.
func serveTextOrImage(t *testing.T, jpg []byte) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".md") {
			w.Write([]byte("# notes"))
			return
		}
		w.Write(jpg)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// serveEmpty answers with a zero-length body.
func serveEmpty(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "0")
	}))
	t.Cleanup(ts.Close)
	return ts
}

// serveStatus answers every path with an error status.
func serveStatus(t *testing.T, code int) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", code)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func callFetch(t *testing.T, args string) (string, bool) {
	t.Helper()
	// fetch reads only, so it needs neither a store nor a chat.
	return callTool(t, nil, "fetch", args, mcphub.CallMeta{})
}

// ---- fetch: read a URL, hand the text back ----

func TestFetchText(t *testing.T) {
	allowWebFetchLoopback(t)
	body := `{"items":[1,2,3]}`
	ts := serve(t, "application/json", []byte(body))

	out, isErr := callFetch(t, `{"url":"`+ts.URL+`/data.json"}`)
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	if out != body {
		t.Fatalf("result = %q, want the raw body %q", out, body)
	}
}

// A text body larger than the tool-result budget reaches the model as a
// rune-safe prefix with the cut announced.
func TestFetchTextTruncated(t *testing.T) {
	allowWebFetchLoopback(t)
	big := strings.Repeat("日", maxOutputBytes) // 3 bytes per rune → 3 MiB
	ts := serve(t, "", []byte(big))

	out, isErr := callFetch(t, `{"url":"`+ts.URL+`/big.txt"}`)
	if isErr {
		t.Fatalf("unexpected tool error: %q", out[:80])
	}
	if len(out) > maxOutputBytes+256 {
		t.Fatalf("result = %d bytes, want <= %d + note", len(out), maxOutputBytes)
	}
	if !strings.Contains(out, "truncated") {
		t.Fatal("result should announce the truncation")
	}
	if !utf8.ValidString(out) {
		t.Fatal("truncation split a rune")
	}
}

// A body the model cannot read is an error naming what it is, not base64
// garbage: giving the user a file from a URL is create_file's job.
func TestFetchBinaryRefused(t *testing.T) {
	allowWebFetchLoopback(t)
	cases := []struct {
		name, ctype string
		body        []byte
		want        string
	}{
		{"image", "image/jpeg", testJPEG(t, 5, 5), "image/jpeg"},
		{"pdf", "application/pdf", []byte("%PDF-1.4\n\x00\xfe\xff binary"), "application/pdf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := serve(t, tc.ctype, tc.body)
			out, isErr := callFetch(t, `{"url":"`+ts.URL+`/x"}`)
			if !isErr {
				t.Fatalf("want an in-band error, got %d bytes", len(out))
			}
			if !strings.Contains(out, "not text") || !strings.Contains(out, tc.want) {
				t.Fatalf("error should say it is not text and name the type: %q", out)
			}
		})
	}
}

func TestFetchErrors(t *testing.T) {
	allowWebFetchLoopback(t)
	emptySrv := serveEmpty(t)
	srvErr := serveStatus(t, http.StatusForbidden)

	cases := []struct{ name, args, want string }{
		{"bad json", `{`, "invalid arguments"},
		{"empty url", `{"url":""}`, "url is required"},
		{"bad scheme", `{"url":"ftp://example.com/x"}`, "not a valid http(s) URL"},
		{"empty body", `{"url":"` + emptySrv.URL + `/x"}`, "empty response"},
		{"server error", `{"url":"` + srvErr.URL + `/x"}`, "403"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, isErr := callFetch(t, tc.args)
			if !isErr {
				t.Fatalf("want in-band error, got success: %q", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("error %q does not contain %q", out, tc.want)
			}
		})
	}
}

// The catalog the engine actually runs: the two file paths and the two local
// tools. attach_file is gone — create_file shows what it creates.
func TestBuiltinFileTools(t *testing.T) {
	names := map[string]bool{}
	for _, e := range Builtin(&fakeFileStore{}, nil).Tools() {
		names[e.Display] = true
	}
	for _, want := range []string{"create_file", "fetch", "code", "time"} {
		if !names[want] {
			t.Fatalf("catalog is missing %q: %v", want, names)
		}
	}
	for _, gone := range []string{"attach_file", "text_file"} {
		if names[gone] {
			t.Fatalf("%q should be gone from the catalog", gone)
		}
	}
}

// No integrated tool's LLM-facing text may point at another tool by name: a
// name in prose is a promise the user can break by turning that tool off, and
// the invisible-file bug this replaced came from exactly that. Only the
// underscore names are checked — "code", "time" and "fetch" are ordinary
// English words that show up in prose ("source code") and would only produce
// false positives.
func TestNoCrossToolReferences(t *testing.T) {
	for _, e := range Builtin(&fakeFileStore{}, nil).Tools() {
		for _, name := range []string{"create_file", "attach_file"} {
			if e.Display == name {
				continue
			}
			if strings.Contains(e.Description, name) {
				t.Errorf("%s description names %s: %s", e.Display, name, e.Description)
			}
		}
	}
}
