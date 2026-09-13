package config

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
)

// Patch is a partial configuration update: only non-nil fields change the
// stored config. Pointer fields distinguish "not provided" (keep current
// value) from an explicit zero value (clear it). Auth is NOT patchable:
// it is driven by the CHATTO_USERNAME / CHATTO_PASSWORD environment
// variables and fixed at startup.
type Patch struct {
	SystemPrompt *string        `json:"system_prompt,omitempty"`
	Provider     *ProviderPatch `json:"provider,omitempty"`
	Models       *ModelsPatch   `json:"models,omitempty"`
	// MCPServers replaces the whole server list when present (empty list
	// removes all servers).
	MCPServers *[]MCPServerConfig `json:"mcp_servers,omitempty"`
	Limits     *LimitsPatch       `json:"limits,omitempty"`
	// ToolDefaults replaces the whole global per-tool default map when
	// present (empty map = every tool falls back to its catalog default).
	ToolDefaults *map[string]bool `json:"tool_defaults,omitempty"`
	// ToolTitles replaces the whole global per-tool user-facing title map
	// when present (empty map = every tool falls back to its catalog title).
	// Values are trimmed and blank ones dropped on write.
	ToolTitles *map[string]string `json:"tool_titles,omitempty"`
}

// ProviderPatch updates the provider endpoint settings.
type ProviderPatch struct {
	BaseURL *string `json:"base_url,omitempty"`
	APIKey  *string `json:"api_key,omitempty"`
}

// ModelsPatch updates the model whitelist, the designated models, and
// per-model metadata.
type ModelsPatch struct {
	Whitelist          *[]string `json:"whitelist,omitempty"`
	DefaultChatModel   *string   `json:"default_chat_model,omitempty"`
	DefaultTaskModel   *string   `json:"default_task_model,omitempty"`
	DefaultVisionModel *string   `json:"default_vision_model,omitempty"`
	// Both optional, like the vision model: reservations for role-specific
	// features that Complete() does not require.
	DefaultDocumentModel *string `json:"default_document_model,omitempty"`
	DefaultAudioModel    *string `json:"default_audio_model,omitempty"`
	// Metas upserts per-model metadata (context window, modalities,
	// reasoning efforts) into the models table alongside the whitelist.
	// Entries are sanitized on write; models dropped from the whitelist
	// lose their stored metadata.
	Metas *[]ModelMeta `json:"metas,omitempty"`
}

// LimitsPatch updates the resource limits.
type LimitsPatch struct {
	UploadMaxFileBytes    *int64 `json:"upload_max_file_bytes,omitempty"`
	MaxToolIterations     *int   `json:"max_tool_iterations,omitempty"`
	MCPCallTimeoutSeconds *int   `json:"mcp_call_timeout_seconds,omitempty"`
}

// ValidationError marks an Update/forceSet rejection the caller can fix
// (bad patch values or broken invariants). API handlers map it to a 400
// carrying the wrapped detail as the user-facing message; persistence
// failures are 500s. It wraps the underlying error so Error() returns the
// clean detail text.
type ValidationError struct{ err error }

func (e *ValidationError) Error() string { return e.err.Error() }
func (e *ValidationError) Unwrap() error { return e.err }

// Update applies patch on top of the current snapshot, then finalizes
// (defaults/sanitizing), validates and persists the result atomically (one
// transaction covering the config rows AND the model-metadata changes). On
// success it swaps in the new snapshot, notifies subscribers and returns
// the new snapshot. On any failure nothing is persisted and the old
// snapshot stays in effect.
func (s *Store) Update(ctx context.Context, patch Patch) (*Config, error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()

	next := s.Get().clone()
	applyPatch(next, patch)
	next.finalize()
	if err := s.sanitizeDesignated(ctx, next, patch); err != nil {
		return nil, err
	}
	if err := next.validate(); err != nil {
		return nil, &ValidationError{err}
	}
	// Per-model metadata lives in the models table, not the config rows:
	// prune metadata of de-whitelisted models and upsert any metadata sent
	// with the patch, in the same transaction as the config rows.
	var pruneKeep *[]string
	if patch.Models != nil && patch.Models.Whitelist != nil {
		pruneKeep = &next.Models.Whitelist
	}
	var metas []ModelMeta
	if patch.Models != nil && patch.Models.Metas != nil {
		metas = append([]ModelMeta(nil), *patch.Models.Metas...)
	}
	if err := s.persist(ctx, next, pruneKeep, metas); err != nil {
		return nil, fmt.Errorf("persist config: %w", err)
	}
	s.notify(next)
	return next, nil
}

// sanitizeDesignated clears a designated model that can't do the job it is
// designated for: the chat and task models must accept "text" input, the
// vision and document models "image", the audio model "audio". Clearing
// rather than rejecting is the point — a broken designation ends up in the
// same state as a missing one, so Complete()
// reports the config as unfinished and the settings overlay stays forced open
// until it is fixed. Metadata lives in the models table, not in the Config
// snapshot, so this reads it; the patch's metas win over the stored rows
// because the settings sheet sends the edited card list in the same patch as
// the designations. (Metas changed behind Update's back —
// POST /api/setup/models — are caught by the next save.)
func (s *Store) sanitizeDesignated(ctx context.Context, c *Config, patch Patch) error {
	designated := []struct {
		kind     string
		id       *string
		modality string
	}{
		{"chat", &c.Models.DefaultChatModel, "text"},
		{"task", &c.Models.DefaultTaskModel, "text"},
		{"vision", &c.Models.DefaultVisionModel, "image"},
		{"document", &c.Models.DefaultDocumentModel, "image"},
		{"audio", &c.Models.DefaultAudioModel, "audio"},
	}
	ids := make([]string, 0, len(designated))
	for _, d := range designated {
		if *d.id != "" {
			ids = append(ids, *d.id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	stored, err := s.ModelMetas(ctx, ids)
	if err != nil {
		return fmt.Errorf("load model metadata: %w", err)
	}
	input := make(map[string][]string, len(stored))
	for _, m := range stored {
		input[m.ModelID] = m.InputModality
	}
	if patch.Models != nil && patch.Models.Metas != nil {
		for _, m := range *patch.Models.Metas {
			// m is a copy: sanitizing it never touches the caller's patch.
			SanitizeMeta(&m)
			input[m.ModelID] = m.InputModality
		}
	}
	for _, d := range designated {
		if *d.id != "" && !slices.Contains(input[*d.id], d.modality) {
			slog.Warn("config: dropping designated model without the input modality its role needs",
				"role", d.kind, "model", *d.id, "needs", d.modality)
			*d.id = ""
		}
	}
	return nil
}

// applyPatch merges the non-nil patch fields into c.
func applyPatch(c *Config, p Patch) {
	if p.SystemPrompt != nil {
		c.SystemPrompt = *p.SystemPrompt
	}
	if p.Provider != nil {
		if p.Provider.BaseURL != nil {
			c.Provider.BaseURL = strings.TrimSpace(*p.Provider.BaseURL)
		}
		if p.Provider.APIKey != nil {
			c.Provider.APIKey = strings.TrimSpace(*p.Provider.APIKey)
		}
	}
	if p.Models != nil {
		if p.Models.Whitelist != nil {
			c.Models.Whitelist = append([]string(nil), *p.Models.Whitelist...)
		}
		if p.Models.DefaultChatModel != nil {
			c.Models.DefaultChatModel = strings.TrimSpace(*p.Models.DefaultChatModel)
		}
		if p.Models.DefaultTaskModel != nil {
			c.Models.DefaultTaskModel = strings.TrimSpace(*p.Models.DefaultTaskModel)
		}
		if p.Models.DefaultVisionModel != nil {
			c.Models.DefaultVisionModel = strings.TrimSpace(*p.Models.DefaultVisionModel)
		}
		if p.Models.DefaultDocumentModel != nil {
			c.Models.DefaultDocumentModel = strings.TrimSpace(*p.Models.DefaultDocumentModel)
		}
		if p.Models.DefaultAudioModel != nil {
			c.Models.DefaultAudioModel = strings.TrimSpace(*p.Models.DefaultAudioModel)
		}
	}
	if p.MCPServers != nil {
		// Header values round-trip through the setup API (GET exposes them,
		// PUT carries them back verbatim), so the patch is authoritative:
		// an empty value clears the header (sanitizeMCPServers drops it).
		c.MCPServers = append([]MCPServerConfig(nil), *p.MCPServers...)
	}
	if p.ToolDefaults != nil {
		// The decoded map is freshly allocated by the JSON decoder and never
		// reused by the caller, so it can be adopted as-is.
		c.ToolDefaults = *p.ToolDefaults
	}
	if p.ToolTitles != nil {
		// Adopted as-is like ToolDefaults; finalize trims the values and drops
		// the blank ones so the stored map stays sparse.
		c.ToolTitles = *p.ToolTitles
	}
	if p.Limits != nil {
		if p.Limits.UploadMaxFileBytes != nil {
			c.Limits.UploadMaxFileBytes = *p.Limits.UploadMaxFileBytes
		}
		if p.Limits.MaxToolIterations != nil {
			c.Limits.MaxToolIterations = *p.Limits.MaxToolIterations
		}
		if p.Limits.MCPCallTimeoutSeconds != nil {
			c.Limits.MCPCallTimeoutSeconds = *p.Limits.MCPCallTimeoutSeconds
		}
	}
}
