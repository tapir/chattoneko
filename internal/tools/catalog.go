package tools

import "chattoneko/internal/config"

// Builtin returns the hardcoded catalog of integrated tools. Add new
// integrated tools to this list — each tool's definition (name, description,
// schema, default toggle, handler) lives in its own file; tools that need
// dependencies (stores) are constructed here with them, so no package-level
// wiring state is needed. files is the attachment store used by tools that
// persist artifacts (create_text_file, show_image); limits supplies the
// live-configured size limits.
func Builtin(files FileStore, limits *config.Store) *Registry {
	return New(
		TimeLocation,
		SimpleCode,
		CreateTextFile(files),
		ShowImage(files, limits),
	)
}
