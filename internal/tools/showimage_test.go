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
	"chattoneko/internal/webimage"
)

// allowWebImageLoopback lets the show_image tests reach their httptest
// servers on 127.0.0.1, which the SSRF blocklist rejects by default.
func allowWebImageLoopback(t *testing.T) {
	t.Helper()
	webimage.AllowLoopbackForTesting(true)
	t.Cleanup(func() { webimage.AllowLoopbackForTesting(false) })
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

func callShowImage(t *testing.T, fs FileStore, args string, meta mcphub.CallMeta) (string, bool) {
	t.Helper()
	r := Builtin(fs, nil)
	out, isErr, err := r.Call(context.Background(), "show_image", args, meta)
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	return out, isErr
}

func TestShowImage(t *testing.T) {
	allowWebImageLoopback(t)
	jpg := testJPEG(t, 40, 30)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(jpg)
	}))
	defer ts.Close()

	fs := &fakeFileStore{}
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}
	out, isErr := callShowImage(t, fs, `{"url":"`+ts.URL+`/pics/cat.jpg"}`, meta)
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

func TestShowImageFilename(t *testing.T) {
	allowWebImageLoopback(t)
	jpg := testJPEG(t, 5, 5)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(jpg)
	}))
	defer ts.Close()
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}

	// Extension forced to .png (stored bytes are PNG regardless of source).
	cases := []struct {
		name, args, want string
	}{
		{"jpg source", `{"url":"` + ts.URL + `/a/photo.jpeg"}`, "photo.png"},
		{"webp source", `{"url":"` + ts.URL + `/a/sticker.webp"}`, "sticker.png"},
		{"png kept", `{"url":"` + ts.URL + `/a/diagram.PNG"}`, "diagram.PNG"},
		{"no extension", `{"url":"` + ts.URL + `/a/img"}`, "img.png"},
		{"root path", `{"url":"` + ts.URL + `"}`, "image.png"},
		{"explicit wins", `{"url":"` + ts.URL + `/a/cat.jpg","filename":"my cat"}`, "my cat.png"},
		{"explicit invalid falls back", `{"url":"` + ts.URL + `/a/cat.jpg","filename":"../evil"}`, "cat.png"},
		{"query stripped", `{"url":"` + ts.URL + `/a/cat.jpg?sig=xyz&exp=1"}`, "cat.png"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeFileStore{}
			out, isErr := callShowImage(t, fs, tc.args, meta)
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
	out, isErr := callShowImage(t, fs, `{"url":"`+ts.URL+`/a/x.jpg","filename":"`+longCJK+`"}`, meta)
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

func TestShowImageErrors(t *testing.T) {
	allowWebImageLoopback(t)
	jpg := testJPEG(t, 5, 5)
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(jpg)
	}))
	defer imgSrv.Close()
	// A bot-protection style interstitial: HTML body, image-ish URL.
	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<html><body>Are you a robot?</body></html>"))
	}))
	defer htmlSrv.Close()

	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}
	cases := []struct {
		name, args, want string
	}{
		{"bad json", `{`, "invalid arguments"},
		{"empty url", `{"url":""}`, "url is required"},
		{"bad scheme", `{"url":"ftp://example.com/x.png"}`, "not a valid http(s) URL"},
		{"html body", `{"url":"` + htmlSrv.URL + `/img.png"}`, "did not return an image"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeFileStore{}
			out, isErr := callShowImage(t, fs, tc.args, meta)
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

	// No chat context.
	fs := &fakeFileStore{}
	out, isErr := callShowImage(t, fs, `{"url":"`+imgSrv.URL+`/x.png"}`, mcphub.CallMeta{})
	if !isErr || !strings.Contains(out, "no chat context") {
		t.Fatalf("want chat-context error, got %v %q", isErr, out)
	}
	// Server error surfaces with the status.
	srvErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srvErr.Close()
	out, isErr = callShowImage(t, fs, `{"url":"`+srvErr.URL+`/x.png"}`, meta)
	if !isErr || !strings.Contains(out, "403") {
		t.Fatalf("want 403 surfaced, got %v %q", isErr, out)
	}
}

func TestShowImageUnsupportedFormat(t *testing.T) {
	allowWebImageLoopback(t)
	// Content-negotiating CDNs may answer our Accept header with a format
	// the pipeline cannot decode (AVIF); the error must name the type.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/avif")
		w.Write([]byte{0x00, 0x00, 0x00, 0x20, 0x66, 0x74, 0x79, 0x70, 0x61, 0x76, 0x69, 0x66, 0x01, 0x02})
	}))
	defer ts.Close()
	fs := &fakeFileStore{}
	out, isErr := callShowImage(t, fs, `{"url":"`+ts.URL+`/x"}`, mcphub.CallMeta{ChatID: "c1", MessageID: "m1"})
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

func TestShowImageNilStore(t *testing.T) {
	out, isErr := callShowImage(t, nil, `{"url":"http://example.com/x.png"}`, mcphub.CallMeta{ChatID: "c", MessageID: "m"})
	if !isErr || !strings.Contains(out, "not available") {
		t.Fatalf("want storage-unavailable error, got isErr=%v %q", isErr, out)
	}
}
