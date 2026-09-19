package tools

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"chattoneko/internal/config"
	"chattoneko/internal/db"
	"chattoneko/internal/mcphub"
	"chattoneko/internal/webfetch"
)

// ---- shared HTTP test helpers ----

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

// testPNG returns a small PNG's bytes, the one format a vision model is sent.
func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// avifBody is an ISOBMFF file with the avif brand, which Go's sniffer reports
// as octet-stream.
func avifBody() []byte {
	return append([]byte("\x00\x00\x00\x1cftypavif\x00\x00\x00\x00avifmif1"), make([]byte, 20)...)
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

// route is one path's answer: a Content-Type and a body.
type route struct {
	ctype string
	body  []byte
}

// serveRoutes answers each listed path with its own Content-Type and body.
func serveRoutes(t *testing.T, routes map[string]route) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt := routes[r.URL.Path]
		if rt.ctype != "" {
			w.Header().Set("Content-Type", rt.ctype)
		}
		w.Write(rt.body)
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

// callFetch runs one fetch call in a chat, which a body that has to be stored
// needs.
func callFetch(t *testing.T, args string) (*fakeFileStore, string, bool) {
	t.Helper()
	return callFetchMeta(t, args, mcphub.CallMeta{ChatID: "c1", MessageID: "m1"})
}

func callFetchMeta(t *testing.T, args string, meta mcphub.CallMeta) (*fakeFileStore, string, bool) {
	t.Helper()
	fs := &fakeFileStore{}
	out, isErr := callTool(t, fs, "fetch", args, meta)
	return fs, out, isErr
}

// staged asserts the one file a fetch stored is shown on NO message: fetch
// stages a file, attach puts it on screen.
func staged(t *testing.T, fs *fakeFileStore) fakeFile {
	t.Helper()
	if len(fs.files) != 1 {
		t.Fatalf("want exactly 1 stored file, got %d", len(fs.files))
	}
	if len(fs.links) != 0 {
		t.Fatalf("a fetched file must not be shown on any message: %+v", fs.links)
	}
	return fs.files[0]
}

// ---- fetch: a text body comes back as text ----

func TestFetchText(t *testing.T) {
	allowWebFetchLoopback(t)
	body := `{"items":[1,2,3]}`
	ts := serve(t, "application/json", []byte(body))

	fs, out, isErr := callFetch(t, `{"url":"`+ts.URL+`/data.json"}`)
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	if out != body {
		t.Fatalf("result = %q, want the raw body %q", out, body)
	}
	if len(fs.files) != 0 {
		t.Fatalf("a text body the model can read must not be stored: %+v", fs.files)
	}
}

// A text body larger than the tool-result budget reaches the model as a
// rune-safe prefix with the cut announced — and stored in full, so what the
// budget cut can still be handed to the user instead of being lost.
func TestFetchTextTruncated(t *testing.T) {
	allowWebFetchLoopback(t)
	big := strings.Repeat("日", maxOutputBytes) // 3 bytes per rune → 3 MiB
	ts := serve(t, "", []byte(big))

	fs, out, isErr := callFetch(t, `{"url":"`+ts.URL+`/big.txt"}`)
	if isErr {
		t.Fatalf("unexpected tool error: %q", out[:80])
	}
	if len(out) > maxOutputBytes+512 {
		t.Fatalf("result = %d bytes, want <= %d + note", len(out), maxOutputBytes)
	}
	if !strings.Contains(out, "truncated") || !utf8.ValidString(out) {
		t.Fatalf("result should announce the truncation on a rune boundary: %q", out[len(out)-200:])
	}
	c := staged(t, fs)
	if c.filename != "big.txt" || c.kind != "text" || string(c.data) != big {
		t.Fatalf("the full body was not stored: name=%q kind=%q size=%d", c.filename, c.kind, c.size)
	}
	if !strings.Contains(out, `</file id="`+c.id+`">`) || !strings.Contains(out, "NOT shown") {
		t.Fatalf("result must hand over the stored file's id and say it is not on screen: %q", out[len(out)-300:])
	}
}

// ---- fetch: anything else is stored, and its id comes back ----

// A body the model cannot read is a file, not an error: stored in the chat
// through the same pipeline an upload goes through, so a JPEG lands as the PNG
// a vision model is sent and a file nothing can read still reaches the user as
// a download. Shown on no message — that is attach's call, which the result
// says out loud.
func TestFetchStoresBinary(t *testing.T) {
	allowWebFetchLoopback(t)
	pdf := []byte("%PDF-1.4\n\x00\xfe\xff binary")
	cases := []struct {
		name, path, ctype string
		body              []byte
		wantFile          string
		wantKind          string
		wantMime          string
		verbatim          bool
	}{
		{"png", "/pics/cat.png", "image/png", testPNG(t, 40, 30), "cat.png", "image", "image/png", false},
		{"jpeg becomes a png", "/pics/cat.jpg", "image/jpeg", testJPEG(t, 40, 30), "cat.png", "image", "image/png", false},
		{"pdf", "/docs/doc.pdf", "application/pdf", pdf, "doc.pdf", "file", "application/pdf", true},
		{"nobody decodes it", "/pics/cat", "image/avif", avifBody(), "cat", "file", "application/octet-stream", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := serveRoutes(t, map[string]route{tc.path: {tc.ctype, tc.body}})
			fs, out, isErr := callFetch(t, `{"url":"`+ts.URL+tc.path+`"}`)
			if isErr {
				t.Fatalf("unexpected tool error: %q", out)
			}
			c := staged(t, fs)
			if c.kind != tc.wantKind || c.mime != tc.wantMime {
				t.Fatalf("stored kind=%q mime=%q, want %q/%q", c.kind, c.mime, tc.wantKind, tc.wantMime)
			}
			if tc.verbatim && string(c.data) != string(tc.body) {
				t.Fatalf("binary not stored verbatim")
			}
			if c.size != int64(len(c.data)) {
				t.Fatalf("size %d != len(data) %d", c.size, len(c.data))
			}
			if !strings.Contains(out, `</file id="`+c.id+`">`) {
				t.Fatalf("result must carry the id in a <file> block: %q", out)
			}
			if !strings.Contains(out, "NOT shown") || !strings.Contains(out, "the attach tool") {
				t.Fatalf("result must say the user cannot see the file yet: %q", out)
			}
			if strings.Contains(out, string(tc.body)) {
				t.Fatalf("result must not echo the downloaded bytes: %q", out)
			}
		})
	}
}

// The stored name comes from the FINAL URL path, with the extension the served
// Content-Type implies when the path has none — and that suffix is what decides
// the file, so an extension-less body served as application/pdf is a document
// while one served as a type nobody knows is a bare download.
func TestFetchStoredName(t *testing.T) {
	allowWebFetchLoopback(t)
	jpeg, pdf, pic := testJPEG(t, 8, 8), []byte("%PDF-1.4\n\x00\xfe\xff binary"), testPNG(t, 8, 8)
	ts := serveRoutes(t, map[string]route{
		"/pics/cat.jpg": {"image/jpeg", jpeg},
		"/pics/img.PNG": {"image/png", pic},
		"/pics/noext":   {"image/jpeg", jpeg},
		"/docs/report":  {"application/pdf", pdf},
		"/api/data":     {"application/json", pdf},
		"/api/odd":      {"application/vnd.custom+json", pdf},
		"/":             {"image/png", jpeg},
	})

	cases := []struct{ path, wantFile, wantMime string }{
		{"/pics/cat.jpg", "cat.png", "image/png"}, // media carry the converted suffix
		{"/pics/img.PNG", "img.PNG", "image/png"}, // a suffix that already agrees is kept
		{"/pics/noext", "noext.png", "image/png"}, // the served type supplies the one that decides
		{"/docs/report", "report.pdf", "application/pdf"},
		{"/api/data", "data.json", "application/octet-stream"},
		{"/api/odd", "odd", "application/octet-stream"},
		{"/", "file.png", "image/png"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			fs, out, isErr := callFetch(t, `{"url":"`+ts.URL+tc.path+`"}`)
			if isErr {
				t.Fatalf("unexpected error: %q", out)
			}
			c := staged(t, fs)
			if c.filename != tc.wantFile || c.mime != tc.wantMime {
				t.Fatalf("got %q/%q, want %q/%q", c.filename, c.mime, tc.wantFile, tc.wantMime)
			}
		})
	}
}

// A long multibyte path segment gets the implied extension appended past the
// 200-byte cap: the truncation must cut on a rune boundary and keep the suffix.
func TestFetchStoredNameCap(t *testing.T) {
	allowWebFetchLoopback(t)
	ts := serve(t, "image/png", testPNG(t, 5, 5))
	fs, out, isErr := callFetch(t, `{"url":"`+ts.URL+`/a/`+strings.Repeat("日", 66)+`a"}`)
	if isErr {
		t.Fatalf("unexpected error: %q", out)
	}
	got := staged(t, fs).filename
	if len(got) > 200 || !utf8.ValidString(got) || !strings.HasSuffix(got, ".png") {
		t.Fatalf("filename = %q (%d bytes, valid UTF-8 %v), want <=200 bytes ending in .png",
			got, len(got), utf8.ValidString(got))
	}
}

// Bytes with a real PNG magic but nonsense after the header are refused: the
// conversion runs on the server, so a picture that will not decode is an
// in-band error rather than a broken thumbnail nobody can explain.
func TestFetchUndecodablePNGIsRefused(t *testing.T) {
	allowWebFetchLoopback(t)
	body := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
	ts := serve(t, "image/png", body)

	fs, out, isErr := callFetch(t, `{"url":"`+ts.URL+`/x.png"}`)
	if !isErr || !strings.Contains(out, "content rejected") {
		t.Fatalf("want a conversion error, got isErr=%v %q", isErr, out)
	}
	if len(fs.files) != 0 {
		t.Fatalf("nothing should have been stored, got %d file(s)", len(fs.files))
	}
}

// image_quantization reaches the conversion of a fetched picture: on stores an
// indexed PNG, off a lossless one.
func TestFetchHonorsQuantizationSetting(t *testing.T) {
	allowWebFetchLoopback(t)
	ts := serve(t, "image/png", testPNG(t, 40, 30))
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}

	for _, quantize := range []bool{true, false} {
		sqlDB, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		t.Cleanup(func() { _ = sqlDB.Close() })
		if err := db.Migrate(sqlDB); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		cfgs, err := config.TestStore(context.Background(), sqlDB,
			config.Config{ImageQuantization: quantize})
		if err != nil {
			t.Fatalf("config store: %v", err)
		}
		fs := &fakeFileStore{}
		out, isErr, err := Builtin(fs, cfgs).Call(context.Background(), "fetch",
			`{"url":"`+ts.URL+`/a/photo.png"}`, meta)
		if err != nil || isErr {
			t.Fatalf("quantize=%v: %v %q", quantize, err, out)
		}
		img, err := png.Decode(bytes.NewReader(fs.files[0].data))
		if err != nil {
			t.Fatalf("quantize=%v: stored bytes are not a PNG: %v", quantize, err)
		}
		if _, paletted := img.(*image.Paletted); paletted != quantize {
			t.Errorf("quantize=%v: stored an indexed PNG = %v", quantize, paletted)
		}
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
			fs, out, isErr := callFetch(t, tc.args)
			if !isErr {
				t.Fatalf("want in-band error, got success: %q", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("error %q does not contain %q", out, tc.want)
			}
			if len(fs.files) != 0 {
				t.Fatal("store must not be called on failure")
			}
		})
	}

	// A binary body with no chat to store it in is the old in-band refusal,
	// naming the type the server sent.
	ts := serve(t, "image/avif", avifBody())
	fs, out, isErr := callFetchMeta(t, `{"url":"`+ts.URL+`/x"}`, mcphub.CallMeta{})
	if !isErr || !strings.Contains(out, "not text") || !strings.Contains(out, "image/avif") {
		t.Fatalf("want a not-text refusal naming the type, got isErr=%v %q", isErr, out)
	}
	if len(fs.files) != 0 {
		t.Fatal("nothing may be stored without a chat")
	}
}

// The catalog the engine runs: attach, fetch, code, time and one specialist per
// file type. The names in the second list must stay out of it.
func TestBuiltinFileTools(t *testing.T) {
	names := map[string]bool{}
	for _, e := range Builtin(&fakeFileStore{}, nil).Tools() {
		names[e.Display] = true
	}
	for _, want := range []string{"attach", "fetch", "code", "time", "vision", "document", "transcribe"} {
		if !names[want] {
			t.Fatalf("catalog is missing %q: %v", want, names)
		}
	}
	for _, gone := range []string{"create_file", "attach_file", "text_file", "agent"} {
		if names[gone] {
			t.Fatalf("%q should be gone from the catalog", gone)
		}
	}
}

// No integrated tool's LLM-facing text may point at another tool by name: a
// name in prose is a promise the user can break by turning that tool off.
// Matched on word boundaries, because "attachment" is an ordinary word the
// specialists cannot do without. Only the names below are checked — "code",
// "time" and "fetch" are ordinary English words that show up in prose ("source
// code").
func TestNoCrossToolReferences(t *testing.T) {
	for _, e := range Builtin(&fakeFileStore{}, nil).Tools() {
		for _, name := range []string{"attach", "create_file", "attach_file"} {
			if e.Display == name {
				continue
			}
			if regexp.MustCompile(`\b` + name + `\b`).MatchString(e.Description) {
				t.Errorf("%s description names %s: %s", e.Display, name, e.Description)
			}
		}
	}
}
