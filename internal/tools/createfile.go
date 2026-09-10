package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"chattoneko/internal/attach"
	"chattoneko/internal/mcphub"
)

// maxFileBytes caps a created file's size. The practical ceiling is far
// lower anyway: the content passes through the model's output tokens.
const maxFileBytes = 5 * 1024 * 1024 // 5 MiB

// CreateFile returns the "create_file" tool: the model writes a complete file
// (text as a string, binary as base64) which is stored as a chat attachment
// and handed back as an id. Storing and showing are separate steps — the file
// becomes visible only when the model passes that id to attach_file, which is
// what lets it create several and show the ones that turned out to matter.
//
// All user/LLM-facing text is hardcoded here — edit in place to change it.
func CreateFile(files FileStore) Tool {
	return Tool{
		Name: "create_file",
		Description: "Create a file for the user. Pass `content` for UTF-8 text (.txt, .md, " +
			".csv, .json, source code, ...) or `content_base64` for binary bytes (a PNG you " +
			"drew, a PDF, a zip) — exactly one of the two, never both. The COMPLETE file must " +
			"be in this one call; partial writes and appends are not possible. The file is " +
			"stored but the user cannot see it yet: you get its attachment id back, and " +
			"passing that id to attach_file is what shows it in the chat. Binary content you " +
			"only fetched is cheaper to keep with fetch(save=true), which stores it without " +
			"round-tripping the bytes through you.",
		Schema: json.RawMessage(`{
		"type": "object",
		"properties": {
			"filename": {
				"type": "string",
				"description": "File name including extension, e.g. notes.md or chart.png. Plain name only, no directories."
			},
			"content": {
				"type": "string",
				"description": "The complete UTF-8 text content of the file. Text files only; omit for binary."
			},
			"content_base64": {
				"type": "string",
				"description": "The complete file bytes, base64-encoded (standard alphabet, no data: URL prefix). Binary files only; omit for text."
			}
		},
		"required": ["filename"],
		"additionalProperties": false
	}`),
		DefaultEnabled: true,
		Title:          "Creating a file…",
		Handler: func(ctx context.Context, argsJSON string, meta mcphub.CallMeta) (string, error) {
			return createFile(ctx, argsJSON, meta, files)
		},
	}
}

func createFile(ctx context.Context, argsJSON string, meta mcphub.CallMeta, files FileStore) (string, error) {
	if files == nil {
		return "", errors.New("file storage is not available")
	}
	var args struct {
		Filename      string `json:"filename"`
		Content       string `json:"content"`
		ContentBase64 string `json:"content_base64"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments JSON: %v", err)
	}
	name, err := attach.CleanFilename(args.Filename)
	if err != nil {
		return "", err
	}
	if meta.ChatID == "" {
		return "", errors.New("no chat context for this call")
	}
	data, err := fileBytes(args.Content, args.ContentBase64)
	if err != nil {
		return "", err
	}
	// Same content rules as user uploads, plus the binary kind they refuse:
	// images are re-encoded to PNG, text is validated, anything else is kept
	// verbatim as a download-only file.
	res, err := attach.ProcessAny(name, data, maxFileBytes)
	if errors.Is(err, attach.ErrTooLarge) {
		return "", fmt.Errorf("content exceeds the %s file size limit", humanSize(maxFileBytes))
	}
	if err != nil {
		return "", fmt.Errorf("content rejected: %v", err)
	}
	m, err := files.CreateAttachment(ctx, meta.ChatID, name, res.Kind, res.Mime, res.Size, res.Data)
	if err != nil {
		return "", fmt.Errorf("store file: %v", err)
	}
	return fmt.Sprintf("Created %q (%s, %s), attachment id %s. The user cannot see it yet — "+
		"call attach_file with that id to show it in the chat, and don't repeat the file "+
		"content in your reply.",
		m.Filename, m.Mime, humanSize(m.Size), m.ID), nil
}

// fileBytes resolves the exactly-one-of content/content_base64 pair. Whitespace
// in the base64 is stripped and missing padding is forgiven — models emit both,
// and a re-call costs a whole round trip.
func fileBytes(content, contentBase64 string) ([]byte, error) {
	switch {
	case content != "" && contentBase64 != "":
		return nil, errors.New("pass either content or content_base64, not both")
	case content != "":
		return []byte(content), nil
	case contentBase64 == "":
		return nil, errors.New("content or content_base64 is required")
	}
	b64 := strings.Join(strings.Fields(contentBase64), "")
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		if data, err = base64.RawStdEncoding.DecodeString(b64); err != nil {
			return nil, fmt.Errorf("content_base64 is not valid base64: %v", err)
		}
	}
	return data, nil
}
