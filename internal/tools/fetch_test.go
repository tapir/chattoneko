package tools

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"chattoneko/internal/mcphub"
	"chattoneko/internal/webfetch"
)

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

func callFetch(t *testing.T, fs FileStore, args string, meta mcphub.CallMeta) (string, bool) {
	t.Helper()
	return callTool(t, fs, "fetch", args, meta)
}

// savedID pulls the attachment id the result hands back.
func savedID(t *testing.T, fs *fakeFileStore) string {
	t.Helper()
	if len(fs.files) != 1 {
		t.Fatalf("want exactly 1 stored file, got %d", len(fs.files))
	}
	return fs.files[0].id
}

func TestFetchImageSaved(t *testing.T) {
	allowWebFetchLoopback(t)
	jpg := testJPEG(t, 40, 30)
	ts := serve(t, "image/jpeg", jpg)

	fs := &fakeFileStore{}
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}
	out, isErr := callFetch(t, fs, `{"url":"`+ts.URL+`/pics/cat.jpg","save":true}`, meta)
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	c := fs.files[0]
	if c.chatID != "c1" {
		t.Fatalf("wrong chat: %+v", c)
	}
	// Saving no longer shows anything: the model decides that with attach_file.
	if len(fs.links) != 0 {
		t.Fatalf("a saved file must stay unlinked: %+v", fs.links)
	}
	// The JPEG source must be converted to PNG (same routine as uploads).
	if c.kind != "image" || c.mime != "image/png" {
		t.Fatalf("wrong kind/mime: %+v", c)
	}
	if c.filename != "cat.png" {
		t.Fatalf("filename not derived from URL: %q", c.filename)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(c.data))
	if err != nil {
		t.Fatalf("stored data is not a PNG: %v", err)
	}
	if cfg.Width != 40 || cfg.Height != 30 {
		t.Fatalf("wrong dimensions: %dx%d", cfg.Width, cfg.Height)
	}
	if c.size != int64(len(c.data)) {
		t.Fatalf("size %d != len(data) %d", c.size, len(c.data))
	}
	// The result hands over the id and the next step, and withholds the bytes.
	if !strings.Contains(out, c.id) || !strings.Contains(out, "attach_file") || !strings.Contains(out, "40x30") {
		t.Fatalf("result missing id/next step/dims: %q", out)
	}
}

// save=false hands a non-text body to the model base64-encoded — a text model
// can't do much with it, but it can pass it on — and stores nothing.
func TestFetchImageBase64(t *testing.T) {
	allowWebFetchLoopback(t)
	jpg := testJPEG(t, 40, 30)
	ts := serve(t, "", jpg)

	fs := &fakeFileStore{}
	out, isErr := callFetch(t, fs, `{"url":"`+ts.URL+`/cat.jpg","save":false}`, mcphub.CallMeta{ChatID: "c1", MessageID: "m1"})
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	if len(fs.files) != 0 {
		t.Fatal("nothing must be stored when save=false")
	}
	header, payload, ok := strings.Cut(out, "\n")
	if !ok {
		t.Fatalf("result has no header line: %q", out[:80])
	}
	if !strings.Contains(header, "base64") || !strings.Contains(header, "40x30") {
		t.Fatalf("header should say what the payload is: %q", header)
	}
	got, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("payload is not base64: %v", err)
	}
	if !bytes.Equal(got, jpg) {
		t.Fatalf("decoded %d bytes, want the %d fetched", len(got), len(jpg))
	}
}

func TestFetchText(t *testing.T) {
	allowWebFetchLoopback(t)
	body := `{"items":[1,2,3]}`
	ts := serve(t, "application/json", []byte(body))
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}

	// save=true: stored as a text attachment, content withheld from the model.
	fs := &fakeFileStore{}
	out, isErr := callFetch(t, fs, `{"url":"`+ts.URL+`/data.json","save":true}`, meta)
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	c := fs.files[0]
	if c.kind != "text" || c.mime != "application/json" || c.filename != "data.json" {
		t.Fatalf("wrong attachment: %+v", c)
	}
	if string(c.data) != body {
		t.Fatalf("stored body = %q, want %q", c.data, body)
	}
	if !strings.Contains(out, c.id) || !strings.Contains(out, "attach_file") || strings.Contains(out, body) {
		t.Fatalf("result should hand over the id and hide the content: %q", out)
	}

	// save omitted: the body goes to the model verbatim and nothing is stored.
	fs2 := &fakeFileStore{}
	out, isErr = callFetch(t, fs2, `{"url":"`+ts.URL+`/data.json"}`, meta)
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	if out != body {
		t.Fatalf("result = %q, want the raw body %q", out, body)
	}
	if len(fs2.files) != 0 {
		t.Fatal("nothing must be stored without save=true")
	}
}

// Text from an extension-less URL (an API path, a bare page) gets the
// extension its Content-Type implies, so the attachment isn't nameless in the
// UI; a name that already has one keeps it.
func TestFetchTextExtension(t *testing.T) {
	allowWebFetchLoopback(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/x/y":
			w.Header().Set("Content-Type", "application/json")
		case "/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		case "/weird":
			w.Header().Set("Content-Type", "application/vnd.custom+json")
		}
		w.Write([]byte(`{"a":1}`))
	}))
	defer ts.Close()
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}

	cases := []struct{ name, path, wantFile, wantMime string }{
		{"json api path", "/repos/x/y", "y.json", "application/json"},
		{"html with params", "/page", "page.html", "text/html"},
		{"unknown type stays bare", "/weird", "weird", "text/plain"},
		{"existing extension kept", "/notes.md", "notes.md", "text/markdown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeFileStore{}
			out, isErr := callFetch(t, fs, `{"url":"`+ts.URL+tc.path+`","save":true}`, meta)
			if isErr {
				t.Fatalf("unexpected error: %q", out)
			}
			if fs.files[0].filename != tc.wantFile || fs.files[0].mime != tc.wantMime {
				t.Fatalf("got %q/%q, want %q/%q", fs.files[0].filename, fs.files[0].mime, tc.wantFile, tc.wantMime)
			}
		})
	}
}

func TestCapName(t *testing.T) {
	if got := capName("short.json"); got != "short.json" {
		t.Fatalf("short name changed: %q", got)
	}
	if want := strings.Repeat("a", 195) + ".json"; capName(strings.Repeat("a", 300)+".json") != want {
		t.Fatal("extension must survive the cut")
	}
	// A dotfile-style name makes filepath.Ext return the whole thing; the
	// extension budget must not go negative (a slice panic) and the result must
	// stay inside the cap and valid UTF-8.
	for _, name := range []string{"." + strings.Repeat("a", 300), strings.Repeat("日", 100) + ".json"} {
		got := capName(name)
		if len(got) > 200 || !utf8.ValidString(got) {
			t.Fatalf("capName(%d bytes) = %d bytes, valid UTF-8 %v", len(name), len(got), utf8.ValidString(got))
		}
	}
}

// A text body larger than the tool-result budget reaches the model as a
// rune-safe prefix with the cut announced.
func TestFetchTextTruncated(t *testing.T) {
	allowWebFetchLoopback(t)
	big := strings.Repeat("日", maxOutputBytes) // 3 bytes per rune → 3 MiB
	ts := serve(t, "", []byte(big))

	out, isErr := callFetch(t, &fakeFileStore{}, `{"url":"`+ts.URL+`/big.txt"}`, mcphub.CallMeta{ChatID: "c", MessageID: "m"})
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

// Binary that is not an image is no longer refused: save=true stores it as a
// download-only file, save=false base64-encodes it for the model.
func TestFetchBinary(t *testing.T) {
	allowWebFetchLoopback(t)
	pdf := []byte("%PDF-1.4\n\x00\xfe\xff binary")
	ts := serve(t, "application/pdf", pdf)
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}

	fs := &fakeFileStore{}
	out, isErr := callFetch(t, fs, `{"url":"`+ts.URL+`/f.pdf","save":true}`, meta)
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	if c := fs.files[0]; c.kind != "file" || c.mime != "application/pdf" || !bytes.Equal(c.data, pdf) {
		t.Fatalf("binary not stored verbatim as a file: %+v", c)
	}
	if !strings.Contains(out, "downloads when clicked") {
		t.Fatalf("result should say how the user will see it: %q", out)
	}

	fs2 := &fakeFileStore{}
	out, isErr = callFetch(t, fs2, `{"url":"`+ts.URL+`/f.pdf"}`, meta)
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	_, payload, _ := strings.Cut(out, "\n")
	got, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || !bytes.Equal(got, pdf) {
		t.Fatalf("base64 payload wrong (err %v): %q", err, out)
	}
	if !strings.Contains(out, "application/pdf") {
		t.Fatalf("header should name the served type: %q", out)
	}
	if len(fs2.files) != 0 {
		t.Fatal("save=false must store nothing")
	}
}

// Truncated base64 is unusable, so a body that doesn't fit the tool-result
// budget is an error pointing at save=true instead of a broken prefix.
func TestFetchBinaryTooLargeToEncode(t *testing.T) {
	allowWebFetchLoopback(t)
	blob := append([]byte{0x00, 0xfe, 0xff}, bytes.Repeat([]byte{0x41}, maxOutputBytes)...) // base64 > 1 MiB
	ts := serve(t, "application/octet-stream", blob)

	out, isErr := callFetch(t, &fakeFileStore{}, `{"url":"`+ts.URL+`/big.bin"}`, mcphub.CallMeta{ChatID: "c", MessageID: "m"})
	if !isErr {
		t.Fatalf("want an error, got %d bytes of base64", len(out))
	}
	if !strings.Contains(out, "save=true") {
		t.Fatalf("error should point at save=true: %q", out)
	}
}

// Bytes that claim to be an image but don't decode are refused rather than
// stored: a corrupt picture is neither a preview nor a useful download.
func TestFetchCorruptImage(t *testing.T) {
	allowWebFetchLoopback(t)
	ts := serve(t, "image/png", append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x00}, 32)...))

	fs := &fakeFileStore{}
	out, isErr := callFetch(t, fs, `{"url":"`+ts.URL+`/x.png","save":true}`, mcphub.CallMeta{ChatID: "c1", MessageID: "m1"})
	if !isErr {
		t.Fatalf("want an error, got %q", out)
	}
	if !strings.Contains(out, "corrupt png image") {
		t.Fatalf("error should name the format: %q", out)
	}
	if len(fs.files) != 0 {
		t.Fatal("store must not be called")
	}
}

func TestFetchFilename(t *testing.T) {
	allowWebFetchLoopback(t)
	jpg := testJPEG(t, 5, 5)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".md") {
			w.Write([]byte("# notes"))
			return
		}
		w.Write(jpg)
	}))
	defer ts.Close()
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}

	// Image names get .png forced on (stored bytes are PNG regardless of source).
	cases := []struct {
		name, args, want string
	}{
		{"jpg source", `{"url":"` + ts.URL + `/a/photo.jpeg","save":true}`, "photo.png"},
		{"webp source", `{"url":"` + ts.URL + `/a/sticker.webp","save":true}`, "sticker.png"},
		{"png kept", `{"url":"` + ts.URL + `/a/diagram.PNG","save":true}`, "diagram.PNG"},
		{"no extension", `{"url":"` + ts.URL + `/a/img","save":true}`, "img.png"},
		{"root path", `{"url":"` + ts.URL + `","save":true}`, "image.png"},
		{"explicit wins", `{"url":"` + ts.URL + `/a/cat.jpg","filename":"my cat","save":true}`, "my cat.png"},
		{"explicit invalid falls back", `{"url":"` + ts.URL + `/a/cat.jpg","filename":"../evil","save":true}`, "cat.png"},
		{"query stripped", `{"url":"` + ts.URL + `/a/cat.jpg?sig=xyz&exp=1","save":true}`, "cat.png"},
		// Text keeps the served name, extension and all.
		{"text keeps name", `{"url":"` + ts.URL + `/a/notes.md","save":true}`, "notes.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeFileStore{}
			out, isErr := callFetch(t, fs, tc.args, meta)
			if isErr {
				t.Fatalf("unexpected error: %q", out)
			}
			if fs.files[0].filename != tc.want {
				t.Fatalf("filename = %q, want %q", fs.files[0].filename, tc.want)
			}
		})
	}

	// A long multibyte explicit name (199 bytes, no extension) gets .png
	// appended past the 200-byte cap: the truncation must cut at a rune
	// boundary and stay valid UTF-8.
	longCJK := strings.Repeat("日", 66) + "a"
	fs := &fakeFileStore{}
	out, isErr := callFetch(t, fs, `{"url":"`+ts.URL+`/a/x.jpg","filename":"`+longCJK+`","save":true}`, meta)
	if isErr {
		t.Fatalf("unexpected error: %q", out)
	}
	got := fs.files[0].filename
	if len(got) > 200 {
		t.Fatalf("filename = %d bytes, want <= 200", len(got))
	}
	if !strings.HasSuffix(got, ".png") {
		t.Fatalf("filename = %q, want .png suffix", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("filename is not valid UTF-8 (split multibyte rune): %q", got)
	}
	if id := savedID(t, fs); !strings.Contains(out, id) {
		t.Fatalf("result should carry the attachment id %q: %q", id, out)
	}
}

func TestFetchErrors(t *testing.T) {
	allowWebFetchLoopback(t)
	jpg := testJPEG(t, 5, 5)
	imgSrv := serve(t, "", jpg)
	emptySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "0")
	}))
	defer emptySrv.Close()

	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}
	cases := []struct {
		name, args, want string
	}{
		{"bad json", `{`, "invalid arguments"},
		{"empty url", `{"url":"","save":true}`, "url is required"},
		{"bad scheme", `{"url":"ftp://example.com/x.png","save":true}`, "not a valid http(s) URL"},
		{"empty body", `{"url":"` + emptySrv.URL + `/x"}`, "empty response"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeFileStore{}
			out, isErr := callFetch(t, fs, tc.args, meta)
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

	// No chat context: fine when nothing is stored, refused when it is.
	fs := &fakeFileStore{}
	out, isErr := callFetch(t, fs, `{"url":"`+imgSrv.URL+`/x.png","save":true}`, mcphub.CallMeta{})
	if !isErr || !strings.Contains(out, "no chat context") {
		t.Fatalf("want chat-context error, got %v %q", isErr, out)
	}
	out, isErr = callFetch(t, fs, `{"url":"`+imgSrv.URL+`/x.png"}`, mcphub.CallMeta{})
	if isErr {
		t.Fatalf("save=false needs no chat context, got %q", out)
	}
	// Server error surfaces with the status.
	srvErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srvErr.Close()
	out, isErr = callFetch(t, fs, `{"url":"`+srvErr.URL+`/x.png","save":true}`, meta)
	if !isErr || !strings.Contains(out, "403") {
		t.Fatalf("want 403 surfaced, got %v %q", isErr, out)
	}
}

func TestFetchNilStore(t *testing.T) {
	out, isErr := callFetch(t, nil, `{"url":"http://example.com/x.png","save":true}`, mcphub.CallMeta{ChatID: "c", MessageID: "m"})
	if !isErr || !strings.Contains(out, "not available") {
		t.Fatalf("want storage-unavailable error, got isErr=%v %q", isErr, out)
	}
}

// The catalog the engine actually runs: both file tools present, text_file gone.
func TestBuiltinFileTools(t *testing.T) {
	names := map[string]bool{}
	for _, e := range Builtin(&fakeFileStore{}, nil).Tools() {
		names[e.Display] = true
	}
	for _, want := range []string{"create_file", "attach_file", "fetch", "code", "time"} {
		if !names[want] {
			t.Fatalf("catalog is missing %q: %v", want, names)
		}
	}
	if names["text_file"] {
		t.Fatal("text_file should be gone: create_file + attach_file replace it")
	}
}
