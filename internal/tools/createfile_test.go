package tools

import (
	"context"
	"strings"
	"testing"

	"chattoneko/internal/mcphub"
	"chattoneko/internal/store"
)

// fakeFileStore records CreateLinkedAttachment calls.
type fakeFileStore struct {
	calls []fakeFileCall
	fail  error
}

type fakeFileCall struct {
	chatID, messageID, filename, kind, mime string
	size                                    int64
	data                                    []byte
}

func (f *fakeFileStore) CreateLinkedAttachment(_ context.Context, chatID, messageID, filename, kind, mime string, size int64, data []byte) (*store.AttachmentMeta, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	f.calls = append(f.calls, fakeFileCall{chatID, messageID, filename, kind, mime, size, data})
	return &store.AttachmentMeta{
		ID: "att-1", ChatID: chatID, MessageID: messageID,
		Filename: filename, Kind: kind, Mime: mime, Size: size,
	}, nil
}

func callCreate(t *testing.T, fs FileStore, args string, meta mcphub.CallMeta) (string, bool) {
	t.Helper()
	r := Builtin(fs, nil)
	out, isErr, err := r.Call(context.Background(), "create_text_file", args, meta)
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	return out, isErr
}

func TestCreateTextFile(t *testing.T) {
	fs := &fakeFileStore{}
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}
	out, isErr := callCreate(t, fs, `{"filename":"notes.md","content":"# hi\n"}`, meta)
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
	if c.filename != "notes.md" || c.kind != "text" || c.mime != "text/markdown" {
		t.Fatalf("wrong meta: %+v", c)
	}
	if string(c.data) != "# hi\n" {
		t.Fatalf("wrong data: %q", c.data)
	}
	// The result must name the file so the model can refer to it.
	if !strings.Contains(out, "notes.md") {
		t.Fatalf("result does not mention filename: %q", out)
	}
}

func TestCreateTextFileValidation(t *testing.T) {
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
		{"no meta", `{"filename":"a.txt","content":"x"}`, mcphub.CallMeta{}, "no chat context"},
		{"binary content", `{"filename":"a.txt","content":"a\u0000b"}`, meta, "rejected"},
		{"too large", `{"filename":"a.txt","content":"` + strings.Repeat("x", maxFileBytes+1) + `"}`, meta, "size limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeFileStore{}
			out, isErr := callCreate(t, fs, tc.args, tc.meta)
			if !isErr {
				t.Fatalf("want in-band error, got success: %q", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("error %q does not contain %q", out, tc.want)
			}
			if len(fs.calls) != 0 {
				t.Fatalf("store must not be called on validation failure")
			}
		})
	}
}

func TestCreateTextFileNilStore(t *testing.T) {
	out, isErr := callCreate(t, nil, `{"filename":"a.txt","content":"x"}`, mcphub.CallMeta{ChatID: "c", MessageID: "m"})
	if !isErr || !strings.Contains(out, "not available") {
		t.Fatalf("want storage-unavailable error, got isErr=%v %q", isErr, out)
	}
}
