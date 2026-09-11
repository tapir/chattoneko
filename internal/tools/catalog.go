package tools

import (
	"context"
	"fmt"

	"chattoneko/internal/config"
	"chattoneko/internal/store"
)

// FileStore is the attachment store create_file works against: it stores the
// file and links it to the assistant message being generated, in one step, so
// a successful call always means the user can see it. *store.Store implements
// it; tests can substitute a fake.
type FileStore interface {
	CreateAttachment(ctx context.Context, chatID, filename, kind, mime string, size int64, data []byte) (*store.AttachmentMeta, error)
	LinkAttachmentToMessage(ctx context.Context, attachmentID, messageID, chatID string) error
}

// Builtin returns the hardcoded catalog of integrated tools. Add new
// integrated tools to this list — each tool's definition (name, description,
// schema, default toggle, handler) lives in its own file; tools that need
// dependencies (stores) are constructed here with them, so no package-level
// wiring state is needed. files is the attachment store create_file uses;
// limits supplies the live-configured size limits for what it downloads.
func Builtin(files FileStore, limits *config.Store) *Registry {
	return New(
		Time,
		Code,
		CreateFile(files, limits),
		Fetch,
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
