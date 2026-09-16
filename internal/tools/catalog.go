package tools

import (
	"context"
	"fmt"

	"chattoneko/internal/config"
	"chattoneko/internal/store"
)

// fileStore is the attachment store the file and specialist tools work
// against: create_file stores a file and links it to the assistant message
// being generated in one step, so a successful call always means the user can
// see it, and the specialists read one back by id. *store.Store implements
// it; tests can substitute a fake.
type fileStore interface {
	CreateAttachment(ctx context.Context, chatID, filename, kind, mime string, size int64, data []byte) (*store.AttachmentMeta, error)
	LinkAttachmentToMessage(ctx context.Context, attachmentID, messageID, chatID string) error
	GetAttachment(ctx context.Context, id string) (*store.Attachment, error)
}

// Builtin returns the catalog of integrated tools. Each tool's definition
// lives in its own file; tools that need dependencies (stores) are constructed
// here with them, so no package-level wiring state is needed. cfgs is the live
// config store: create_file reads the size limits from it, the specialists
// their designated models.
func Builtin(files fileStore, cfgs *config.Store) *registry {
	ts := []tool{Time, Code, CreateFile(files, cfgs), Fetch}
	return newRegistry(append(ts, Specialists(files, cfgs)...)...)
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
