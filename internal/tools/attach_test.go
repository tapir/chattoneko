package tools

import (
	"context"
	"encoding/base64"
	"fmt"
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

func TestAttachText(t *testing.T) {
	fs := &fakeFileStore{}
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}
	out, isErr := callTool(t, fs, "attach", `{"filename":"notes.md","content":"# hi\n"}`, meta)
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
	if strings.Contains(out, "# hi") {
		t.Fatalf("result must not echo the content: %q", out)
	}
}

// Binary goes in as base64 and lands as a download-only file — except real
// image bytes, which the shared pipeline re-encodes to JPEG like every upload.
func TestAttachBinary(t *testing.T) {
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}
	pdf := []byte("%PDF-1.4\n\x00\xfe\xff binary")

	fs := &fakeFileStore{}
	out, isErr := callTool(t, fs, "attach",
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
			out, isErr := callTool(t, fs, "attach", `{"filename":"a.pdf","content_base64":"`+
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

// ---- the ids source: show a file this chat already holds ----

// stagedIn seeds the fake store with files a previous tool left behind, and
// returns their ids.
func stagedIn(fs *fakeFileStore, chatID string, files ...[2]string) []string {
	ids := make([]string, 0, len(files))
	for _, f := range files {
		m, err := fs.CreateAttachment(context.Background(), chatID, f[0], f[1], "text/plain", 3, []byte("abc"))
		if err != nil {
			panic(err)
		}
		ids = append(ids, m.ID)
	}
	return ids
}

func TestAttachStoredIDs(t *testing.T) {
	fs := &fakeFileStore{}
	ids := stagedIn(fs, "c1", [2]string{"cat.png", "image"}, [2]string{"notes.md", "text"})
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m9"}

	out, isErr := callTool(t, fs, "attach", `{"ids":["`+ids[0]+`"," `+ids[1]+` "]}`, meta)
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	// Both are on the message being generated, in the order asked for, and
	// nothing new was stored.
	if len(fs.links) != 2 || fs.links[0] != [2]string{ids[0], "m9"} || fs.links[1] != [2]string{ids[1], "m9"} {
		t.Fatalf("wrong links: %+v", fs.links)
	}
	if len(fs.files) != 2 {
		t.Fatalf("the ids source must not store anything, got %d files", len(fs.files))
	}
	for _, id := range ids {
		if !strings.Contains(out, `<file name=`) || !strings.Contains(out, `</file id="`+id+`">`) {
			t.Fatalf("result must carry every id in its own <file> block: %q", out)
		}
	}
	if !strings.Contains(out, "Attached") || !strings.Contains(out, "shown on your reply") {
		t.Fatalf("result should say the files are on screen: %q", out)
	}
	// The kind label comes from the stored attachment, not from the argument.
	if !strings.Contains(out, "shows inline as an image") || !strings.Contains(out, "opens as a text preview") {
		t.Fatalf("result should say how each file shows: %q", out)
	}
}

// A wrong id fails the whole call BEFORE anything is linked, so the model never
// gets a half-shown reply it cannot reason about.
func TestAttachStoredIDsVetted(t *testing.T) {
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m9"}
	cases := []struct {
		name string
		seed func(fs *fakeFileStore) string
		want string
	}{
		{"unknown id", func(*fakeFileStore) string { return "nope" }, "no attachment with id"},
		{"another chat's id", func(fs *fakeFileStore) string { return stagedIn(fs, "c2", [2]string{"x.txt", "text"})[0] }, "no attachment with id"},
		{"blank id", func(fs *fakeFileStore) string { return "  " }, "must be an attachment id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeFileStore{}
			good := stagedIn(fs, "c1", [2]string{"good.txt", "text"})[0]
			out, isErr := callTool(t, fs, "attach", `{"ids":["`+good+`","`+tc.seed(fs)+`"]}`, meta)
			if !isErr || !strings.Contains(out, tc.want) {
				t.Fatalf("want %q, got isErr=%v %q", tc.want, isErr, out)
			}
			if len(fs.links) != 0 {
				t.Fatalf("nothing may be linked when an id is rejected: %+v", fs.links)
			}
		})
	}
}

func TestAttachValidation(t *testing.T) {
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
		{"ids and content", `{"ids":["a"],"content":"x"}`, meta, "not both"},
		{"ids and base64", `{"ids":["a"],"content_base64":"eA=="}`, meta, "not both"},
		{"empty ids", `{"ids":[]}`, meta, "at least one attachment id"},
		{"bad base64", `{"filename":"a.bin","content_base64":"not base64 at all!"}`, meta, "not valid base64"},
		{"blank base64", `{"filename":"a.bin","content_base64":"   "}`, meta, "content rejected"},
		{"too large", `{"filename":"a.txt","content":"` + strings.Repeat("x", config.DefaultUploadMaxFileBytes+1) + `"}`, meta, "size limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeFileStore{}
			out, isErr := callTool(t, fs, "attach", tc.args, tc.meta)
			if !isErr {
				t.Fatalf("want in-band error, got success: %q", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("error %q does not contain %q", out, tc.want)
			}
			if len(fs.files) != 0 || len(fs.links) != 0 {
				t.Fatalf("nothing may be stored or shown on validation failure: %+v %+v", fs.files, fs.links)
			}
		})
	}
}

// The size cap is the configured upload limit, so a file the model writes
// itself gets no larger a budget than a user's upload.
func TestAttachWrittenContentHonorsConfiguredLimit(t *testing.T) {
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
		out, isErr, err := Builtin(&fakeFileStore{}, cfgs).Call(context.Background(), "attach",
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

// A failed link is an error, not a silent invisible file: nothing the model
// could call next recovers it.
func TestAttachLinkFailure(t *testing.T) {
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}
	fs := &fakeFileStore{linkErr: fmt.Errorf("boom")}
	out, isErr := callTool(t, fs, "attach", `{"filename":"a.txt","content":"x"}`, meta)
	if !isErr || !strings.Contains(out, "could not be shown") {
		t.Fatalf("want a not-shown error, got isErr=%v %q", isErr, out)
	}

	// The ids source reports the same, naming the file it could not show.
	fs = &fakeFileStore{linkErr: fmt.Errorf("boom")}
	id := stagedIn(fs, "c1", [2]string{"cat.png", "image"})[0]
	out, isErr = callTool(t, fs, "attach", `{"ids":["`+id+`"]}`, meta)
	if !isErr || !strings.Contains(out, "cat.png") || !strings.Contains(out, "could not be shown") {
		t.Fatalf("want a not-shown error naming the file, got isErr=%v %q", isErr, out)
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
