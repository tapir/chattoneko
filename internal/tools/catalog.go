package tools

import (
	"context"
	"fmt"

	"chattoneko/internal/config"
	"chattoneko/internal/store"
)

// FileStore is the attachment store the file tools work against: create_file
// and fetch(save=true) write a file, attach_file puts one on screen. Files are
// stored unlinked and only become visible when attach_file links them to the
// assistant message, so nothing here needs to know about messages except the
// link itself. *store.Store implements it; tests can substitute a fake.
type FileStore interface {
	CreateAttachment(ctx context.Context, chatID, filename, kind, mime string, size int64, data []byte) (*store.AttachmentMeta, error)
	GetAttachmentMeta(ctx context.Context, id string) (*store.AttachmentMeta, error)
	LinkAttachmentToMessage(ctx context.Context, attachmentID, messageID, chatID string) error
}

// Builtin returns the hardcoded catalog of integrated tools. Add new
// integrated tools to this list — each tool's definition (name, description,
// schema, default toggle, handler) lives in its own file; tools that need
// dependencies (stores) are constructed here with them, so no package-level
// wiring state is needed. files is the attachment store used by the file
// tools (create_file, attach_file, fetch); limits supplies the
// live-configured size limits.
func Builtin(files FileStore, limits *config.Store) *Registry {
	return New(
		Time,
		Code,
		CreateFile(files),
		AttachFile(files),
		Fetch(files, limits),
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
