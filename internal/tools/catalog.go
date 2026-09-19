package tools

import (
	"context"
	"fmt"

	"chattoneko/internal/config"
	"chattoneko/internal/store"
)

// fileStore is the attachment store the file and specialist tools work
// against: fetch stores a file it downloaded, attach stores one the model
// wrote and links files to the assistant message being generated in one step,
// so a successful attach always means the user can see the file, and the
// specialists read one back by id. *store.Store implements it; tests can
// substitute a fake.
type fileStore interface {
	CreateAttachment(ctx context.Context, chatID, filename, kind, mime string, size int64, data []byte) (*store.AttachmentMeta, error)
	LinkAttachmentToMessage(ctx context.Context, attachmentID, messageID, chatID string) error
	GetAttachment(ctx context.Context, id string) (*store.Attachment, error)
}

// Builtin returns the catalog of integrated tools. Each tool's definition
// lives in its own file; tools that need dependencies (stores) are constructed
// here with them, so no package-level wiring state is needed. cfgs is the live
// config store: attach and fetch read the size limits from it, speak and the
// specialists their designated models.
func Builtin(files fileStore, cfgs *config.Store) *registry {
	ts := []tool{Time, Code, Attach(files, cfgs), Fetch(files, cfgs), Speak(files, cfgs)}
	r := newRegistry(append(ts, Specialists(files, cfgs)...)...)
	r.cfgs = cfgs
	return r
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
