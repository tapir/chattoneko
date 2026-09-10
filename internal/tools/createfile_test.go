package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"chattoneko/internal/mcphub"
	"chattoneko/internal/store"
)

// fakeFileStore is the in-memory FileStore the file-tool tests run against:
// created files get sequential ids, GetAttachmentMeta answers from what was
// created, and links are recorded instead of written.
type fakeFileStore struct {
	files []fakeFile
	links [][2]string // attachment id -> message id
	// fail switches: one error per method, so a test can fail any step.
	createErr, metaErr, linkErr error
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

func (f *fakeFileStore) GetAttachmentMeta(_ context.Context, id string) (*store.AttachmentMeta, error) {
	if f.metaErr != nil {
		return nil, f.metaErr
	}
	for _, file := range f.files {
		if file.id == id {
			return &store.AttachmentMeta{
				ID: file.id, ChatID: file.chatID, Filename: file.filename,
				Kind: file.kind, Mime: file.mime, Size: file.size,
			}, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeFileStore) LinkAttachmentToMessage(_ context.Context, attachmentID, messageID, _ string) error {
	if f.linkErr != nil {
		return f.linkErr
	}
	f.links = append(f.links, [2]string{attachmentID, messageID})
	return nil
}

func callTool(t *testing.T, fs FileStore, tool, args string, meta mcphub.CallMeta) (string, bool) {
	t.Helper()
	out, isErr, err := Builtin(fs, nil).Call(context.Background(), tool, args, meta)
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
	if len(fs.files) != 1 {
		t.Fatalf("want 1 stored file, got %d", len(fs.files))
	}
	c := fs.files[0]
	// Stored UNLINKED: showing it is attach_file's job, not create_file's.
	if c.chatID != "c1" || len(fs.links) != 0 {
		t.Fatalf("created file must be unlinked: %+v links %+v", c, fs.links)
	}
	if c.filename != "notes.md" || c.kind != "text" || c.mime != "text/markdown" {
		t.Fatalf("wrong meta: %+v", c)
	}
	if string(c.data) != "# hi\n" {
		t.Fatalf("wrong data: %q", c.data)
	}
	// The result hands the id over and says the file isn't visible yet.
	if !strings.Contains(out, c.id) || !strings.Contains(out, "attach_file") {
		t.Fatalf("result must return the id and name the next step: %q", out)
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
	if c := fs.files[0]; c.kind != "file" || string(c.data) != string(pdf) {
		t.Fatalf("binary not stored verbatim: %+v", c)
	}
	if !strings.Contains(out, "attach_file") {
		t.Fatalf("result should point at attach_file: %q", out)
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
		{"no content", `{"filename":"a.txt"}`, meta, "content or content_base64 is required"},
		{"empty content", `{"filename":"a.txt","content":""}`, meta, "content or content_base64 is required"},
		{"both contents", `{"filename":"a.txt","content":"x","content_base64":"eA=="}`, meta, "not both"},
		{"bad base64", `{"filename":"a.bin","content_base64":"not base64 at all!"}`, meta, "not valid base64"},
		{"blank base64", `{"filename":"a.bin","content_base64":"   "}`, meta, "content rejected"},
		{"too large", `{"filename":"a.txt","content":"` + strings.Repeat("x", maxFileBytes+1) + `"}`, meta, "size limit"},
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

func TestCreateFileNilStore(t *testing.T) {
	out, isErr := callTool(t, nil, "create_file", `{"filename":"a.txt","content":"x"}`, mcphub.CallMeta{ChatID: "c", MessageID: "m"})
	if !isErr || !strings.Contains(out, "not available") {
		t.Fatalf("want storage-unavailable error, got isErr=%v %q", isErr, out)
	}
}
