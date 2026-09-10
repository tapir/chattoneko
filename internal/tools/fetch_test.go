package tools

import (
	"bytes"
	"context"
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

func callFetch(t *testing.T, fs FileStore, args string, meta mcphub.CallMeta) (string, bool) {
	t.Helper()
	r := Builtin(fs, nil)
	out, isErr, err := r.Call(context.Background(), "fetch", args, meta)
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	return out, isErr
}

func TestFetchImageShown(t *testing.T) {
	allowWebFetchLoopback(t)
	jpg := testJPEG(t, 40, 30)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(jpg)
	}))
	defer ts.Close()

	fs := &fakeFileStore{}
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}
	out, isErr := callFetch(t, fs, `{"url":"`+ts.URL+`/pics/cat.jpg","show":true}`, meta)
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	if len(fs.calls) != 1 {
		t.Fatalf("want 1 store call, got %d", len(fs.calls))
	}
	c := fs.calls[0]
	if c.chatID != "c1" || c.messageID != "m1" {
		t.Fatalf("wrong linkage: %+v", c)
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
	// The result tells the model the image is displayed (so it stops pasting links).
	if !strings.Contains(out, "displayed inline") || !strings.Contains(out, "40x30") {
		t.Fatalf("result missing display notice/dims: %q", out)
	}
}

// show=false hands an image's metadata to the model instead of pixels —
// tool results are text — and stores nothing.
func TestFetchImageHidden(t *testing.T) {
	allowWebFetchLoopback(t)
	jpg := testJPEG(t, 40, 30)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(jpg)
	}))
	defer ts.Close()

	fs := &fakeFileStore{}
	out, isErr := callFetch(t, fs, `{"url":"`+ts.URL+`/cat.jpg","show":false}`, mcphub.CallMeta{ChatID: "c1", MessageID: "m1"})
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	if len(fs.calls) != 0 {
		t.Fatal("nothing must be stored when show=false")
	}
	if !strings.Contains(out, "40x30") || !strings.Contains(out, "show=true") {
		t.Fatalf("result should describe the image and point at show=true: %q", out)
	}
	if strings.Contains(out, string(jpg)) {
		t.Fatal("raw image bytes must not be returned to the model")
	}
}

func TestFetchText(t *testing.T) {
	allowWebFetchLoopback(t)
	body := `{"items":[1,2,3]}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	defer ts.Close()
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}

	// show=true: stored as a text attachment, content withheld from the model.
	fs := &fakeFileStore{}
	out, isErr := callFetch(t, fs, `{"url":"`+ts.URL+`/data.json","show":true}`, meta)
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	if len(fs.calls) != 1 {
		t.Fatalf("want 1 store call, got %d", len(fs.calls))
	}
	c := fs.calls[0]
	if c.kind != "text" || c.mime != "application/json" || c.filename != "data.json" {
		t.Fatalf("wrong attachment: %+v", c)
	}
	if string(c.data) != body {
		t.Fatalf("stored body = %q, want %q", c.data, body)
	}
	if !strings.Contains(out, "attached to your reply") || strings.Contains(out, body) {
		t.Fatalf("result should announce the attachment and hide the content: %q", out)
	}

	// show=false: the body goes to the model verbatim and nothing is stored.
	fs2 := &fakeFileStore{}
	out, isErr = callFetch(t, fs2, `{"url":"`+ts.URL+`/data.json","show":false}`, meta)
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	if out != body {
		t.Fatalf("result = %q, want the raw body %q", out, body)
	}
	if len(fs2.calls) != 0 {
		t.Fatal("nothing must be stored when show=false")
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
			out, isErr := callFetch(t, fs, `{"url":"`+ts.URL+tc.path+`","show":true}`, meta)
			if isErr {
				t.Fatalf("unexpected error: %q", out)
			}
			if fs.calls[0].filename != tc.wantFile || fs.calls[0].mime != tc.wantMime {
				t.Fatalf("got %q/%q, want %q/%q", fs.calls[0].filename, fs.calls[0].mime, tc.wantFile, tc.wantMime)
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
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(big))
	}))
	defer ts.Close()

	out, isErr := callFetch(t, &fakeFileStore{}, `{"url":"`+ts.URL+`/big.txt","show":false}`, mcphub.CallMeta{ChatID: "c", MessageID: "m"})
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

// Binary that is not an image is refused both ways: it can't be rendered in
// the chat and it can't be quoted to the model.
func TestFetchBinary(t *testing.T) {
	allowWebFetchLoopback(t)
	pdf := []byte("%PDF-1.4\n\x00\xfe\xff binary")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write(pdf)
	}))
	defer ts.Close()
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}

	for _, show := range []string{"true", "false"} {
		fs := &fakeFileStore{}
		out, isErr := callFetch(t, fs, `{"url":"`+ts.URL+`/f.pdf","show":`+show+`}`, meta)
		if !isErr {
			t.Fatalf("show=%s: want error, got %q", show, out)
		}
		if !strings.Contains(out, "application/pdf") {
			t.Fatalf("show=%s: error should name the served type: %q", show, out)
		}
		if len(fs.calls) != 0 {
			t.Fatalf("show=%s: store must not be called", show)
		}
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
		{"jpg source", `{"url":"` + ts.URL + `/a/photo.jpeg","show":true}`, "photo.png"},
		{"webp source", `{"url":"` + ts.URL + `/a/sticker.webp","show":true}`, "sticker.png"},
		{"png kept", `{"url":"` + ts.URL + `/a/diagram.PNG","show":true}`, "diagram.PNG"},
		{"no extension", `{"url":"` + ts.URL + `/a/img","show":true}`, "img.png"},
		{"root path", `{"url":"` + ts.URL + `","show":true}`, "image.png"},
		{"explicit wins", `{"url":"` + ts.URL + `/a/cat.jpg","filename":"my cat","show":true}`, "my cat.png"},
		{"explicit invalid falls back", `{"url":"` + ts.URL + `/a/cat.jpg","filename":"../evil","show":true}`, "cat.png"},
		{"query stripped", `{"url":"` + ts.URL + `/a/cat.jpg?sig=xyz&exp=1","show":true}`, "cat.png"},
		// Text keeps the served name, extension and all.
		{"text keeps name", `{"url":"` + ts.URL + `/a/notes.md","show":true}`, "notes.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeFileStore{}
			out, isErr := callFetch(t, fs, tc.args, meta)
			if isErr {
				t.Fatalf("unexpected error: %q", out)
			}
			if fs.calls[0].filename != tc.want {
				t.Fatalf("filename = %q, want %q", fs.calls[0].filename, tc.want)
			}
		})
	}

	// A long multibyte explicit name (199 bytes, no extension) gets .png
	// appended past the 200-byte cap: the truncation must cut at a rune
	// boundary and stay valid UTF-8.
	longCJK := strings.Repeat("日", 66) + "a"
	fs := &fakeFileStore{}
	out, isErr := callFetch(t, fs, `{"url":"`+ts.URL+`/a/x.jpg","filename":"`+longCJK+`","show":true}`, meta)
	if isErr {
		t.Fatalf("unexpected error: %q", out)
	}
	got := fs.calls[0].filename
	if len(got) > 200 {
		t.Fatalf("filename = %d bytes, want <= 200", len(got))
	}
	if !strings.HasSuffix(got, ".png") {
		t.Fatalf("filename = %q, want .png suffix", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("filename is not valid UTF-8 (split multibyte rune): %q", got)
	}
}

func TestFetchErrors(t *testing.T) {
	allowWebFetchLoopback(t)
	jpg := testJPEG(t, 5, 5)
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(jpg)
	}))
	defer imgSrv.Close()
	emptySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "0")
	}))
	defer emptySrv.Close()

	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}
	cases := []struct {
		name, args, want string
	}{
		{"bad json", `{`, "invalid arguments"},
		{"empty url", `{"url":"","show":true}`, "url is required"},
		{"bad scheme", `{"url":"ftp://example.com/x.png","show":true}`, "not a valid http(s) URL"},
		{"empty body", `{"url":"` + emptySrv.URL + `/x","show":false}`, "empty response"},
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
			if len(fs.calls) != 0 {
				t.Fatal("store must not be called on failure")
			}
		})
	}

	// No chat context: fine when nothing is stored, refused when it is.
	fs := &fakeFileStore{}
	out, isErr := callFetch(t, fs, `{"url":"`+imgSrv.URL+`/x.png","show":true}`, mcphub.CallMeta{})
	if !isErr || !strings.Contains(out, "no chat context") {
		t.Fatalf("want chat-context error, got %v %q", isErr, out)
	}
	// Server error surfaces with the status.
	srvErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srvErr.Close()
	out, isErr = callFetch(t, fs, `{"url":"`+srvErr.URL+`/x.png","show":true}`, meta)
	if !isErr || !strings.Contains(out, "403") {
		t.Fatalf("want 403 surfaced, got %v %q", isErr, out)
	}
}

func TestFetchUnsupportedFormat(t *testing.T) {
	allowWebFetchLoopback(t)
	// Content-negotiating CDNs may answer our Accept header with a format
	// the pipeline cannot decode (AVIF); the error must name the type.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/avif")
		w.Write([]byte{0x00, 0x00, 0x00, 0x20, 0x66, 0x74, 0x79, 0x70, 0x61, 0x76, 0x69, 0x66, 0x01, 0x02})
	}))
	defer ts.Close()
	fs := &fakeFileStore{}
	out, isErr := callFetch(t, fs, `{"url":"`+ts.URL+`/x","show":true}`, mcphub.CallMeta{ChatID: "c1", MessageID: "m1"})
	if !isErr {
		t.Fatalf("want error, got %q", out)
	}
	if !strings.Contains(out, "image/avif") {
		t.Fatalf("error should name the served type: %q", out)
	}
	if len(fs.calls) != 0 {
		t.Fatal("store must not be called")
	}
}

func TestFetchNilStore(t *testing.T) {
	out, isErr := callFetch(t, nil, `{"url":"http://example.com/x.png","show":true}`, mcphub.CallMeta{ChatID: "c", MessageID: "m"})
	if !isErr || !strings.Contains(out, "not available") {
		t.Fatalf("want storage-unavailable error, got isErr=%v %q", isErr, out)
	}
}
