package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"chattoneko/internal/attach"
	"chattoneko/internal/mcphub"
	"chattoneko/internal/store"
)

// AttachFile returns the "attach_file" tool: it links existing attachments to
// the assistant message being generated, which is what makes them appear in
// the chat (the engine publishes every attachment of that message as it goes).
// Linking is a join-table insert, so showing a file that is already on another
// message — an image the user uploaded, whose id the model can see in the
// injected <file> block — leaves the original where it is.
//
// All user/LLM-facing text is hardcoded here — edit in place to change it.
func AttachFile(files FileStore) Tool {
	return Tool{
		Name: "attach_file",
		Description: "Show files in the chat as part of your reply, by attachment id: an image " +
			"appears inline, a text file opens as a preview, anything else downloads when " +
			"clicked. Use it with the ids returned by create_file and fetch(save=true) — this " +
			"is the only way the user sees those files. An id from a <file id=\"...\"> block in " +
			"the conversation (a text file the user sent) works too, and stays attached where it " +
			"was. Attaching is not a substitute for describing what you did: say what the files " +
			"are in your reply, without repeating their content.",
		Schema: json.RawMessage(`{
		"type": "object",
		"properties": {
			"ids": {
				"type": "array",
				"items": {"type": "string"},
				"minItems": 1,
				"description": "Attachment ids to show on your reply."
			}
		},
		"required": ["ids"],
		"additionalProperties": false
	}`),
		DefaultEnabled: true,
		Title:          "Attaching files…",
		Handler: func(ctx context.Context, argsJSON string, meta mcphub.CallMeta) (string, error) {
			return attachFiles(ctx, argsJSON, meta, files)
		},
	}
}

func attachFiles(ctx context.Context, argsJSON string, meta mcphub.CallMeta, files FileStore) (string, error) {
	if files == nil {
		return "", errors.New("file storage is not available")
	}
	var args struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments JSON: %v", err)
	}
	if len(args.IDs) == 0 {
		return "", errors.New("ids is required")
	}
	if meta.ChatID == "" || meta.MessageID == "" {
		return "", errors.New("no chat context for this call")
	}
	var shown, missed []string
	for _, id := range args.IDs {
		id = strings.TrimSpace(id)
		m, err := files.GetAttachmentMeta(ctx, id)
		// A foreign id must not link: the query is chat-scoped, so it would
		// silently do nothing and the model would believe the file is showing.
		if errors.Is(err, store.ErrNotFound) || (err == nil && m.ChatID != meta.ChatID) {
			missed = append(missed, fmt.Sprintf("%s (no such file in this chat)", id))
			continue
		}
		if err != nil {
			return "", fmt.Errorf("look up attachment %s: %v", id, err)
		}
		if err := files.LinkAttachmentToMessage(ctx, id, meta.MessageID, meta.ChatID); err != nil {
			return "", fmt.Errorf("attach %q: %v", m.Filename, err)
		}
		shown = append(shown, fmt.Sprintf("%q (%s, %s, %s)", m.Filename, m.Mime, humanSize(m.Size), kindLabel(m.Kind)))
	}
	if len(shown) == 0 {
		return "", fmt.Errorf("nothing attached: %s", strings.Join(missed, ", "))
	}
	out := fmt.Sprintf("Now visible on your reply: %s. Don't repeat these files or their content in your reply.",
		strings.Join(shown, ", "))
	if len(missed) > 0 {
		out += " Not attached: " + strings.Join(missed, ", ") + "."
	}
	return out, nil
}

// kindLabel says what the user is about to see, so the model can describe the
// file instead of guessing how the chat rendered it.
func kindLabel(kind string) string {
	switch kind {
	case attach.KindImage:
		return "shown inline as an image"
	case attach.KindText:
		return "opens as a text preview"
	default:
		return "downloads when clicked"
	}
}
