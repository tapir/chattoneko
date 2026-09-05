package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"chattoneko/internal/attach"
	"chattoneko/internal/mcphub"
	"chattoneko/internal/store"
)

// FileStore persists tool-generated files as chat attachments. *store.Store
// implements it; tests can substitute a fake.
type FileStore interface {
	CreateLinkedAttachment(ctx context.Context, chatID, messageID, filename, kind, mime string, size int64, data []byte) (*store.AttachmentMeta, error)
}

// maxFileBytes caps a created file's size. The practical ceiling is far
// lower anyway: the content passes through the model's output tokens.
const maxFileBytes = 5 * 1024 * 1024 // 5 MiB

// CreateTextFile returns the "create_text_file" tool bound to the given
// file store: the model writes a complete text file which is stored as an
// attachment linked to the assistant message that produced it, and surfaces
// in the UI as a download link on that reply. Content passes through the
// model's output tokens, so this is inherently for small/medium TEXT files
// — binary formats are rejected (same content validation as user uploads
// via attach.Process).
//
// All user/LLM-facing text is hardcoded here — edit in place to change it.
func CreateTextFile(files FileStore) Tool {
	return Tool{
		Name: "create_text_file",
		Description: "Create a UTF-8 text file that is attached to your reply as a download " +
			"link for the user. Use it when the user asks for a downloadable/saveable file " +
			"(.txt, .md, .csv, .json, source code, ...). Provide the COMPLETE file content " +
			"in one call; partial writes or appends are not possible. Text only — no binary " +
			"formats (images, PDFs, archives).",
		Schema: json.RawMessage(`{
		"type": "object",
		"properties": {
			"filename": {
				"type": "string",
				"description": "File name including extension, e.g. notes.md. Plain name only, no directories."
			},
			"content": {
				"type": "string",
				"description": "The complete UTF-8 text content of the file."
			}
		},
		"required": ["filename", "content"],
		"additionalProperties": false
	}`),
		DefaultEnabled: true,
		Handler: func(ctx context.Context, argsJSON string, meta mcphub.CallMeta) (string, error) {
			return createTextFile(ctx, argsJSON, meta, files)
		},
	}
}

func createTextFile(ctx context.Context, argsJSON string, meta mcphub.CallMeta, files FileStore) (string, error) {
	if files == nil {
		return "", errors.New("file storage is not available")
	}
	var args struct {
		Filename string `json:"filename"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments JSON: %v", err)
	}
	name, err := attach.CleanFilename(args.Filename)
	if err != nil {
		return "", err
	}
	if meta.ChatID == "" || meta.MessageID == "" {
		return "", errors.New("no chat context for this call")
	}
	// Same content rules as user uploads: UTF-8 text sniffing, size cap,
	// mime from the extension whitelist.
	res, err := attach.Process(name, []byte(args.Content), maxFileBytes)
	if errors.Is(err, attach.ErrTooLarge) {
		return "", fmt.Errorf("content exceeds the %s file size limit", humanSize(maxFileBytes))
	}
	if err != nil {
		return "", fmt.Errorf("content rejected: %v", err)
	}
	if res.Kind != attach.KindText {
		return "", errors.New("only text content can be written; binary formats are not supported")
	}
	m, err := files.CreateLinkedAttachment(ctx, meta.ChatID, meta.MessageID, name, res.Kind, res.Mime, res.Size, res.Data)
	if err != nil {
		return "", fmt.Errorf("store file: %v", err)
	}
	return fmt.Sprintf("Created %q (%s, %s). It is attached to your reply as a download link — don't repeat the file content in your reply unless asked.",
		m.Filename, m.Mime, humanSize(m.Size)), nil
}

// humanSize renders a byte count for the model/user ("342 B", "1.2 KB", "3.4 MB").
func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
