package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"strings"
	"testing"
	"unicode/utf8"

	"chattoneko/internal/config"
	"chattoneko/internal/db"
	"chattoneko/internal/mcphub"
	"chattoneko/internal/store"
)

// fakeFileStore is the in-memory fileStore the file-tool tests run against:
// created files get sequential ids and links are recorded instead of written.
type fakeFileStore struct {
	files []fakeFile
	links [][2]string // attachment id -> message id
	// fail switches: one error per method, so a test can fail any step.
	createErr, linkErr, getErr error
}

type fakeFile struct {
	id, chatID, filename, kind, mime string
	size                             int64
	data                             []byte
}

func (f *fakeFileStore) CreateAttachment(_ context.Context, chatID, filename, kind, mime string, size int64, data []byte) (*store.AttachmentMeta, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	file := fakeFile{
		id:       fmt.Sprintf("att-%d", len(f.files)+1),
		chatID:   chatID,
		filename: filename,
		kind:     kind,
		mime:     mime,
		size:     size,
		data:     data,
	}
	f.files = append(f.files, file)
	return &store.AttachmentMeta{
		ID: file.id, ChatID: chatID, Filename: filename, Kind: kind, Mime: mime, Size: size,
	}, nil
}

func (f *fakeFileStore) LinkAttachmentToMessage(_ context.Context, attachmentID, messageID, _ string) error {
	if f.linkErr != nil {
		return f.linkErr
	}
	f.links = append(f.links, [2]string{attachmentID, messageID})
	return nil
}

func (f *fakeFileStore) GetAttachment(_ context.Context, id string) (*store.Attachment, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	for _, file := range f.files {
		if file.id != id {
			continue
		}
		return &store.Attachment{
			AttachmentMeta: store.AttachmentMeta{
				ID: file.id, ChatID: file.chatID, Filename: file.filename,
				Kind: file.kind, Mime: file.mime, Size: file.size,
			},
			Data: file.data,
		}, nil
	}
	return nil, store.ErrNotFound
}

// shownOn asserts the one file created by a call was linked to the message
// that asked for it — storing and showing are one step.
func shownOn(t *testing.T, fs *fakeFileStore, messageID string) fakeFile {
	t.Helper()
	if len(fs.files) != 1 {
		t.Fatalf("want exactly 1 stored file, got %d", len(fs.files))
	}
	if len(fs.links) != 1 || fs.links[0] != [2]string{fs.files[0].id, messageID} {
		t.Fatalf("file not shown on %s: %+v", messageID, fs.links)
	}
	return fs.files[0]
}

func callTool(t *testing.T, fs fileStore, name, args string, meta mcphub.CallMeta) (string, bool) {
	t.Helper()
	out, isErr, err := Builtin(fs, nil).Call(context.Background(), name, args, meta)
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	return out, isErr
}

func TestCreateFileText(t *testing.T) {
	fs := &fakeFileStore{}
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}
	out, isErr := callTool(t, fs, "create_file", `{"filename":"notes.md","content":"# hi\n"}`, meta)
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	c := shownOn(t, fs, "m1")
	if c.chatID != "c1" || c.filename != "notes.md" || c.kind != "text" || c.mime != "text/markdown" {
		t.Fatalf("wrong meta: %+v", c)
	}
	if string(c.data) != "# hi\n" {
		t.Fatalf("wrong data: %q", c.data)
	}
	// The result says the file is visible and hands over its id in the <file>
	// shape the specialists ask for, so a later call can pick the same file up.
	if !strings.Contains(out, "shown on your reply") || !strings.Contains(out, "text preview") {
		t.Fatalf("result should say the file is on screen: %q", out)
	}
	if !strings.Contains(out, `<file name="notes.md" id="`+c.id+`" type="text">`) ||
		!strings.HasSuffix(out, `</file id="`+c.id+`">`) {
		t.Fatalf("result must carry the attachment id in a <file> block: %q", out)
	}
	if strings.Contains(out, "attach_file") {
		t.Fatalf("result must not name another tool: %q", out)
	}
	if strings.Contains(out, "# hi") {
		t.Fatalf("result must not echo the content: %q", out)
	}
}

// Binary goes in as base64 and lands as a download-only file — except real
// image bytes, which the shared pipeline re-encodes to PNG like every upload.
func TestCreateFileBinary(t *testing.T) {
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}
	pdf := []byte("%PDF-1.4\n\x00\xfe\xff binary")

	fs := &fakeFileStore{}
	out, isErr := callTool(t, fs, "create_file",
		`{"filename":"report.pdf","content_base64":"`+base64.StdEncoding.EncodeToString(pdf)+`"}`, meta)
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	if c := shownOn(t, fs, "m1"); c.kind != "file" || string(c.data) != string(pdf) {
		t.Fatalf("binary not stored verbatim: %+v", c)
	}
	if !strings.Contains(out, "downloads when clicked") {
		t.Fatalf("result should say how the user sees it: %q", out)
	}

	// Whitespace and missing padding are forgiven: models emit both.
	for name, enc := range map[string]string{
		"newlines":   base64.StdEncoding.EncodeToString(pdf)[:8] + "\n" + base64.StdEncoding.EncodeToString(pdf)[8:],
		"unpadded":   strings.TrimRight(base64.StdEncoding.EncodeToString(pdf), "="),
		"dataURLish": " " + base64.StdEncoding.EncodeToString(pdf) + " ",
	} {
		t.Run(name, func(t *testing.T) {
			fs := &fakeFileStore{}
			out, isErr := callTool(t, fs, "create_file", `{"filename":"a.pdf","content_base64":"`+
				strings.ReplaceAll(enc, "\n", `\n`)+`"}`, meta)
			if isErr {
				t.Fatalf("unexpected tool error: %q", out)
			}
			if string(fs.files[0].data) != string(pdf) {
				t.Fatalf("decoded bytes wrong: %q", fs.files[0].data)
			}
		})
	}
}

func TestCreateFileValidation(t *testing.T) {
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}
	cases := []struct {
		name string
		args string
		meta mcphub.CallMeta
		want string // substring of the in-band error
	}{
		{"bad json", `{`, meta, "invalid arguments"},
		{"empty filename", `{"filename":"","content":"x"}`, meta, "filename is required"},
		{"path traversal", `{"filename":"../etc/passwd","content":"x"}`, meta, "plain name"},
		{"backslash", `{"filename":"a\\b.txt","content":"x"}`, meta, "plain name"},
		{"dotdot", `{"filename":"..","content":"x"}`, meta, "invalid filename"},
		{"control chars", `{"filename":"a\u0000b.txt","content":"x"}`, meta, "control characters"},
		{"long name", `{"filename":"` + strings.Repeat("a", 201) + `.txt","content":"x"}`, meta, "too long"},
		{"no chat", `{"filename":"a.txt","content":"x"}`, mcphub.CallMeta{}, "no chat context"},
		{"no message", `{"filename":"a.txt","content":"x"}`, mcphub.CallMeta{ChatID: "c1"}, "no chat context"},
		{"no source", `{"filename":"a.txt"}`, meta, "content or content_base64 is required"},
		{"empty content", `{"filename":"a.txt","content":""}`, meta, "content or content_base64 is required"},
		{"both contents", `{"filename":"a.txt","content":"x","content_base64":"eA=="}`, meta, "not both"},
		{"url and content", `{"filename":"a.txt","content":"x","url":"http://example.com/a.txt"}`, meta, "not both"},
		{"url and base64", `{"filename":"a.bin","content_base64":"eA==","url":"http://example.com/a.bin"}`, meta, "not both"},
		{"bad base64", `{"filename":"a.bin","content_base64":"not base64 at all!"}`, meta, "not valid base64"},
		{"blank base64", `{"filename":"a.bin","content_base64":"   "}`, meta, "content rejected"},
		{"too large", `{"filename":"a.txt","content":"` + strings.Repeat("x", config.DefaultUploadMaxFileBytes+1) + `"}`, meta, "size limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeFileStore{}
			out, isErr := callTool(t, fs, "create_file", tc.args, tc.meta)
			if !isErr {
				t.Fatalf("want in-band error, got success: %q", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("error %q does not contain %q", out, tc.want)
			}
			if len(fs.files) != 0 {
				t.Fatalf("store must not be called on validation failure")
			}
		})
	}
}

// The size cap is the configured upload limit for both sources, so a file the
// model writes itself gets no larger a budget than a user's upload.
func TestCreateFileWrittenContentHonorsConfiguredLimit(t *testing.T) {
	sqlDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cfgs, err := config.TestStore(context.Background(), sqlDB, config.Config{
		Limits: config.LimitsConfig{UploadMaxFileBytes: 100},
	})
	if err != nil {
		t.Fatalf("config store: %v", err)
	}
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}
	write := func(n int) (string, bool) {
		out, isErr, err := Builtin(&fakeFileStore{}, cfgs).Call(context.Background(), "create_file",
			`{"filename":"a.txt","content":"`+strings.Repeat("x", n)+`"}`, meta)
		if err != nil {
			t.Fatalf("transport error: %v", err)
		}
		return out, isErr
	}

	out, isErr := write(101)
	if !isErr || !strings.Contains(out, "size limit") {
		t.Fatalf("want a size-limit refusal, got isErr=%v %q", isErr, out)
	}
	if out, isErr = write(100); isErr {
		t.Fatalf("a file within the cap was refused: %q", out)
	}
}

// image_quantization reaches the conversion: on stores an indexed PNG, off a
// lossless one.
func TestCreateFileHonorsQuantizationSetting(t *testing.T) {
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
		out, isErr, err := Builtin(fs, cfgs).Call(context.Background(), "create_file",
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

// A failed link is an error, not a silent invisible file: nothing the model
// could call next recovers it.
func TestCreateFileLinkFailure(t *testing.T) {
	fs := &fakeFileStore{linkErr: fmt.Errorf("boom")}
	out, isErr := callTool(t, fs, "create_file", `{"filename":"a.txt","content":"x"}`,
		mcphub.CallMeta{ChatID: "c1", MessageID: "m1"})
	if !isErr || !strings.Contains(out, "could not be shown") {
		t.Fatalf("want a not-shown error, got isErr=%v %q", isErr, out)
	}
}

// ---- the url source: download a file and hand it over ----

func TestCreateFileFromURLImage(t *testing.T) {
	allowWebFetchLoopback(t)
	ts := serve(t, "image/png", testPNG(t, 40, 30))

	fs := &fakeFileStore{}
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}
	out, isErr := callTool(t, fs, "create_file", `{"url":"`+ts.URL+`/pics/cat.png"}`, meta)
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	c := shownOn(t, fs, "m1")
	// Converted to PNG exactly as the browser converts an upload, and the name
	// follows the bytes, not the URL.
	if c.kind != "image" || c.mime != "image/png" || c.filename != "cat.png" {
		t.Fatalf("wrong attachment: kind=%q mime=%q name=%q", c.kind, c.mime, c.filename)
	}
	if c.size != int64(len(c.data)) {
		t.Fatalf("size %d != len(data) %d", c.size, len(c.data))
	}
	if !strings.Contains(out, ts.URL) || !strings.Contains(out, "inline as an image") {
		t.Fatalf("result should carry the source and how it shows: %q", out)
	}
}

// A JPEG a tool fetches is a picture, not a download: it is re-encoded to PNG
// like every other raster format, which also makes it a mime a vision model is
// sent. The upload path is stricter — see attach's tests.
func TestCreateFileFromURLJPEGIsAnImage(t *testing.T) {
	allowWebFetchLoopback(t)
	ts := serve(t, "image/jpeg", testJPEG(t, 40, 30))

	fs := &fakeFileStore{}
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}
	out, isErr := callTool(t, fs, "create_file", `{"url":"`+ts.URL+`/pics/cat.jpg"}`, meta)
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	c := shownOn(t, fs, "m1")
	if c.kind != "image" || c.mime != "image/png" || c.filename != "cat.png" {
		t.Fatalf("wrong attachment: kind=%q mime=%q name=%q", c.kind, c.mime, c.filename)
	}
	if !strings.Contains(out, "inline as an image") {
		t.Fatalf("result should say how the user will see it: %q", out)
	}
}

// A file no browser here can show is still handed over as a download: the model
// found it on the web, and the user can have it whatever it is.
func TestCreateFileFromURLKeepsUnsupportedAsDownload(t *testing.T) {
	allowWebFetchLoopback(t)
	ts := serve(t, "image/avif", avifBody())

	fs := &fakeFileStore{}
	out, isErr := callTool(t, fs, "create_file", `{"url":"`+ts.URL+`/pics/cat.avif"}`,
		mcphub.CallMeta{ChatID: "c1", MessageID: "m1"})
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	c := shownOn(t, fs, "m1")
	if c.kind != "file" || c.mime != "application/octet-stream" {
		t.Fatalf("wrong attachment: kind=%q mime=%q", c.kind, c.mime)
	}
	if !strings.Contains(out, "downloads when clicked") {
		t.Fatalf("result should say how it shows: %q", out)
	}
}

// avifBody is an ISOBMFF file with the avif brand, which Go's sniffer reports
// as octet-stream.
func avifBody() []byte {
	return append([]byte("\x00\x00\x00\x1cftypavif\x00\x00\x00\x00avifmif1"), make([]byte, 20)...)
}

func TestCreateFileFromURLText(t *testing.T) {
	allowWebFetchLoopback(t)
	body := `{"items":[1,2,3]}`
	ts := serve(t, "application/json", []byte(body))

	fs := &fakeFileStore{}
	out, isErr := callTool(t, fs, "create_file", `{"url":"`+ts.URL+`/data.json"}`,
		mcphub.CallMeta{ChatID: "c1", MessageID: "m1"})
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	c := shownOn(t, fs, "m1")
	if c.kind != "text" || c.mime != "application/json" || c.filename != "data.json" || string(c.data) != body {
		t.Fatalf("wrong attachment: %+v", c)
	}
	// The bytes never route through the model.
	if strings.Contains(out, body) {
		t.Fatalf("result must not echo the downloaded content: %q", out)
	}
}

// Text from an extension-less URL (an API path, a bare page) gets the
// extension its Content-Type implies, so the attachment isn't nameless in the
// UI; a name that already has one keeps it.
func TestCreateFileFromURLExtension(t *testing.T) {
	allowWebFetchLoopback(t)
	ts := servePaths(t, map[string]string{
		"/repos/x/y": "application/json",
		"/page":      "text/html; charset=utf-8",
		"/weird":     "application/vnd.custom+json",
	}, []byte(`{"a":1}`))
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
			out, isErr := callTool(t, fs, "create_file", `{"url":"`+ts.URL+tc.path+`"}`, meta)
			if isErr {
				t.Fatalf("unexpected error: %q", out)
			}
			if fs.files[0].filename != tc.wantFile || fs.files[0].mime != tc.wantMime {
				t.Fatalf("got %q/%q, want %q/%q", fs.files[0].filename, fs.files[0].mime, tc.wantFile, tc.wantMime)
			}
		})
	}
}

func TestCreateFileFromURLFilename(t *testing.T) {
	allowWebFetchLoopback(t)
	ts := serveTextOrImage(t, testPNG(t, 5, 5))
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}

	// Media names carry the suffix of what the conversion produced, so every
	// picture ends as .png — unless its own suffix already says that.
	cases := []struct{ name, args, want string }{
		{"jpg source", `{"url":"` + ts.URL + `/a/photo.jpeg"}`, "photo.png"},
		{"webp source", `{"url":"` + ts.URL + `/a/sticker.webp"}`, "sticker.png"},
		{"png re-encoded", `{"url":"` + ts.URL + `/a/diagram.PNG"}`, "diagram.PNG"}, // the suffix already agrees
		{"no extension", `{"url":"` + ts.URL + `/a/img"}`, "img.png"},
		{"root path", `{"url":"` + ts.URL + `"}`, "file.png"},
		{"explicit wins", `{"url":"` + ts.URL + `/a/cat.jpg","filename":"my cat"}`, "my cat.png"},
		{"explicit invalid falls back", `{"url":"` + ts.URL + `/a/cat.jpg","filename":"../evil"}`, "cat.png"},
		{"query stripped", `{"url":"` + ts.URL + `/a/cat.jpg?sig=xyz&exp=1"}`, "cat.png"},
		// Text keeps the served name, extension and all.
		{"text keeps name", `{"url":"` + ts.URL + `/a/notes.md"}`, "notes.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeFileStore{}
			out, isErr := callTool(t, fs, "create_file", tc.args, meta)
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
	out, isErr := callTool(t, fs, "create_file", `{"url":"`+ts.URL+`/a/x.jpg","filename":"`+longCJK+`"}`, meta)
	if isErr {
		t.Fatalf("unexpected error: %q", out)
	}
	got := fs.files[0].filename
	if len(got) > 200 || !utf8.ValidString(got) || !strings.HasSuffix(got, ".png") {
		t.Fatalf("filename = %q (%d bytes, valid UTF-8 %v), want <=200 bytes ending in .png",
			got, len(got), utf8.ValidString(got))
	}
}

// Binary that is not an image is stored verbatim as a download-only file.
func TestCreateFileFromURLBinary(t *testing.T) {
	allowWebFetchLoopback(t)
	pdf := []byte("%PDF-1.4\n\x00\xfe\xff binary")
	ts := serve(t, "application/pdf", pdf)

	fs := &fakeFileStore{}
	out, isErr := callTool(t, fs, "create_file", `{"url":"`+ts.URL+`/f.pdf"}`,
		mcphub.CallMeta{ChatID: "c1", MessageID: "m1"})
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	if c := shownOn(t, fs, "m1"); c.kind != "file" || c.mime != "application/pdf" || string(c.data) != string(pdf) {
		t.Fatalf("binary not stored verbatim as a file: %+v", c)
	}
	if !strings.Contains(out, "downloads when clicked") {
		t.Fatalf("result should say how the user will see it: %q", out)
	}
}

// Bytes with a real PNG magic but nonsense after the header are refused: the
// conversion runs on the server, so a picture that will not decode is an
// in-band error rather than a broken thumbnail nobody can explain. ffmpeg's own
// diagnosis rides along, which is why only the wrapper is asserted here.
func TestCreateFileFromURLUndecodablePNGIsRefused(t *testing.T) {
	allowWebFetchLoopback(t)
	body := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
	ts := serve(t, "image/png", body)

	fs := &fakeFileStore{}
	out, isErr := callTool(t, fs, "create_file", `{"url":"`+ts.URL+`/x.png"}`,
		mcphub.CallMeta{ChatID: "c1", MessageID: "m1"})
	if !isErr || !strings.Contains(out, "content rejected") {
		t.Fatalf("want a conversion error, got isErr=%v %q", isErr, out)
	}
	if len(fs.files) != 0 {
		t.Fatalf("nothing should have been stored, got %d file(s)", len(fs.files))
	}
}

func TestCreateFileFromURLErrors(t *testing.T) {
	allowWebFetchLoopback(t)
	emptySrv := serveEmpty(t)
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}

	cases := []struct{ name, args, want string }{
		{"bad scheme", `{"url":"ftp://example.com/x.png"}`, "not a valid http(s) URL"},
		{"empty body", `{"url":"` + emptySrv.URL + `/x"}`, "empty response"},
		{"no chat", `{"url":"` + emptySrv.URL + `/x"}`, "no chat context"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeFileStore{}
			m := meta
			if tc.name == "no chat" {
				m = mcphub.CallMeta{}
			}
			out, isErr := callTool(t, fs, "create_file", tc.args, m)
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

	// A server error surfaces with its status.
	srvErr := serveStatus(t, 403)
	out, isErr := callTool(t, &fakeFileStore{}, "create_file", `{"url":"`+srvErr.URL+`/x.png"}`, meta)
	if !isErr || !strings.Contains(out, "403") {
		t.Fatalf("want 403 surfaced, got %v %q", isErr, out)
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
