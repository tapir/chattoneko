package tools

import (
	"context"
	"fmt"

	"chattoneko/internal/config"
	"chattoneko/internal/store"
)

// fileStore is the attachment store the file and agent tools work against:
// create_file stores a file and links it to the assistant message being
// generated in one step, so a successful call always means the user can see
// it; agent reads one back by id. *store.Store implements it; tests can
// substitute a fake.
type fileStore interface {
	CreateAttachment(ctx context.Context, chatID, filename, kind, mime string, size int64, data []byte) (*store.AttachmentMeta, error)
	LinkAttachmentToMessage(ctx context.Context, attachmentID, messageID, chatID string) error
	GetAttachment(ctx context.Context, id string) (*store.Attachment, error)
}

// Builtin returns the hardcoded catalog of integrated tools. Add new
// integrated tools to this list — each tool's definition (name, description,
// schema, default toggle, handler) lives in its own file; tools that need
// dependencies (stores) are constructed here with them, so no package-level
// wiring state is needed. cfgs is the live config store: create_file reads the
// size limits from it, agent the designated specialist models.
func Builtin(files fileStore, cfgs *config.Store) *registry {
	return newRegistry(
		Time,
		Code,
		CreateFile(files, cfgs),
		Fetch,
		Agent(files, cfgs),
	)
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
