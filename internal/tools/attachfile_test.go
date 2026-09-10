package tools

import (
	"errors"
	"strings"
	"testing"

	"chattoneko/internal/mcphub"
)

// createOne stores a file through create_file and returns its attachment id,
// so the attach tests exercise the real two-step flow.
func createOne(t *testing.T, fs *fakeFileStore, chatID, filename, content string) string {
	t.Helper()
	out, isErr := callTool(t, fs, "create_file",
		`{"filename":"`+filename+`","content":"`+content+`"}`, mcphub.CallMeta{ChatID: chatID, MessageID: "m0"})
	if isErr {
		t.Fatalf("create_file failed: %q", out)
	}
	return fs.files[len(fs.files)-1].id
}

func TestAttachFile(t *testing.T) {
	fs := &fakeFileStore{}
	id := createOne(t, fs, "c1", "notes.md", "# hi")

	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}
	out, isErr := callTool(t, fs, "attach_file", `{"ids":["`+id+`"]}`, meta)
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	if len(fs.links) != 1 || fs.links[0] != [2]string{id, "m1"} {
		t.Fatalf("links = %+v, want the file on the assistant message", fs.links)
	}
	// The model is told what the user now sees, so it describes instead of guessing.
	if !strings.Contains(out, "notes.md") || !strings.Contains(out, "text preview") {
		t.Fatalf("result should name the file and how it shows: %q", out)
	}

	// Attaching the same file again is one link, not an error.
	if _, isErr := callTool(t, fs, "attach_file", `{"ids":["`+id+`"]}`, meta); isErr {
		t.Fatal("re-attaching must be harmless")
	}
}

// Several ids in one call: each links, ids are trimmed, and the result lists
// them in call order (the chat itself sorts chips by creation time).
func TestAttachFileSeveral(t *testing.T) {
	fs := &fakeFileStore{}
	a := createOne(t, fs, "c1", "a.md", "a")
	b := createOne(t, fs, "c1", "b.md", "b")

	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}
	out, isErr := callTool(t, fs, "attach_file", `{"ids":["`+b+`"," `+a+` "]}`, meta)
	if isErr {
		t.Fatalf("unexpected tool error: %q", out)
	}
	if len(fs.links) != 2 || fs.links[0][0] != b || fs.links[1][0] != a {
		t.Fatalf("links = %+v, want b then a (ids trimmed)", fs.links)
	}
	if strings.Index(out, "b.md") > strings.Index(out, "a.md") {
		t.Fatalf("result should list the files in the order given: %q", out)
	}
}

func TestAttachFileFailures(t *testing.T) {
	meta := mcphub.CallMeta{ChatID: "c1", MessageID: "m1"}

	t.Run("unknown id", func(t *testing.T) {
		fs := &fakeFileStore{}
		out, isErr := callTool(t, fs, "attach_file", `{"ids":["nope"]}`, meta)
		if !isErr || !strings.Contains(out, "nothing attached") {
			t.Fatalf("want an all-failed error, got isErr=%v %q", isErr, out)
		}
		if len(fs.links) != 0 {
			t.Fatal("nothing must be linked")
		}
	})

	t.Run("other chat's file", func(t *testing.T) {
		fs := &fakeFileStore{}
		foreign := createOne(t, fs, "c2", "secret.md", "x")
		out, isErr := callTool(t, fs, "attach_file", `{"ids":["`+foreign+`"]}`, meta)
		if !isErr {
			t.Fatalf("want an error for a foreign id, got %q", out)
		}
		if len(fs.links) != 0 {
			t.Fatal("a foreign attachment must not be linked")
		}
	})

	t.Run("partial success", func(t *testing.T) {
		fs := &fakeFileStore{}
		good := createOne(t, fs, "c1", "ok.md", "x")
		out, isErr := callTool(t, fs, "attach_file", `{"ids":["`+good+`","missing"]}`, meta)
		if isErr {
			t.Fatalf("the good id must still attach: %q", out)
		}
		if len(fs.links) != 1 || fs.links[0][0] != good {
			t.Fatalf("links = %+v, want only the good file", fs.links)
		}
		if !strings.Contains(out, "Not attached: missing") {
			t.Fatalf("result should report the id that failed: %q", out)
		}
	})

	t.Run("no ids", func(t *testing.T) {
		out, isErr := callTool(t, &fakeFileStore{}, "attach_file", `{"ids":[]}`, meta)
		if !isErr || !strings.Contains(out, "ids is required") {
			t.Fatalf("want an ids-required error, got isErr=%v %q", isErr, out)
		}
	})

	t.Run("no message context", func(t *testing.T) {
		out, isErr := callTool(t, &fakeFileStore{}, "attach_file", `{"ids":["a"]}`, mcphub.CallMeta{ChatID: "c1"})
		if !isErr || !strings.Contains(out, "no chat context") {
			t.Fatalf("want a chat-context error, got isErr=%v %q", isErr, out)
		}
	})

	t.Run("nil store", func(t *testing.T) {
		out, isErr := callTool(t, nil, "attach_file", `{"ids":["a"]}`, meta)
		if !isErr || !strings.Contains(out, "not available") {
			t.Fatalf("want a storage-unavailable error, got isErr=%v %q", isErr, out)
		}
	})

	t.Run("link failure", func(t *testing.T) {
		fs := &fakeFileStore{}
		id := createOne(t, fs, "c1", "a.md", "x")
		fs.linkErr = errors.New("db went away")
		out, isErr := callTool(t, fs, "attach_file", `{"ids":["`+id+`"]}`, meta)
		if !isErr || !strings.Contains(out, "db went away") {
			t.Fatalf("want the store error surfaced, got isErr=%v %q", isErr, out)
		}
	})
}
