package engine

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"chattoneko/internal/attach"
	"chattoneko/internal/provider"
	"chattoneko/internal/store"
	"chattoneko/internal/vision"
)

// describeTimeout bounds one vision-model description call; a slow provider
// stalls at most one image, then the image is sent as-is.
const describeTimeout = 60 * time.Second

// chatParams loads the chat and derives provider.GenParams from its persisted
// per-chat settings. The loaded chat is returned alongside so callers don't
// have to load it again. The error is propagated verbatim (callers must
// distinguish store.ErrNotFound and context cancellation themselves).
func (e *Engine) chatParams(ctx context.Context, chatID string) (*store.Chat, provider.GenParams, error) {
	chat, err := e.store.GetChat(ctx, chatID)
	if err != nil {
		return nil, provider.GenParams{}, err
	}
	p := provider.GenParams{
		Model:           chat.Model,
		ReasoningEffort: chat.Params.ReasoningEffort,
	}
	if p.Model == "" {
		p.Model = e.cfg.Get().Models.DefaultChatModel
	}
	if p.Model == "" {
		// Fresh install before setup completes: no model to run on. Fail the
		// generation cleanly instead of sending an empty model to the provider.
		return nil, provider.GenParams{}, errors.New("no chat model configured yet")
	}
	return chat, p, nil
}

// buildProviderMessages converts persisted history (+ effective system
// prompt + attachments) into normalized provider messages. Attachment blobs
// don't change mid-generation, so fetches are memoized in attCache (pass nil
// for a one-shot build) instead of re-reading every blob on every tool-loop
// iteration.
//
// Image attachments go out as PNG data URLs when the chat model accepts
// images. When it doesn't, each image is replaced by a vision-model text
// description (generated once and cached on the attachment row); when no
// vision model is configured or the call fails, the raw image is sent
// anyway as the fallback.
func (e *Engine) buildProviderMessages(ctx context.Context, chat *store.Chat, msgs []*store.Message, attCache map[string]*store.Attachment) ([]provider.Message, error) {
	out := make([]provider.Message, 0, len(msgs)+1)
	if sp := e.SystemPrompt(); sp != "" {
		out = append(out, provider.Message{Role: "system", Content: sp})
	}
	seesImages := e.modelSeesImages(ctx, chat.Model)
	for _, m := range msgs {
		switch m.Role {
		case store.RoleUser:
			content := m.Content
			var images []provider.Image
			for _, a := range m.Attachments {
				att, ok := attCache[a.ID]
				if !ok {
					var err error
					att, err = e.store.GetAttachment(ctx, a.ID)
					if err != nil {
						return nil, err
					}
					if attCache != nil {
						attCache[a.ID] = att
					}
				}
				switch att.Kind {
				case attach.KindImage:
					if !seesImages {
						if desc := e.imageDescription(ctx, att); desc != "" {
							content += "\n\n" + attach.SerializeImageDescription(att.Filename, att.ID, desc)
							continue
						}
						// No description available (no vision model
						// configured or the call failed): fall back to
						// sending the image itself.
					}
					images = append(images, provider.Image{Data: att.Data})
				case attach.KindText:
					content += "\n\n" + attach.SerializeText(att.Filename, att.ID, string(att.Data))
				}
			}
			out = append(out, provider.Message{Role: "user", Content: content, Images: images})
		case store.RoleAssistant:
			// Skip a completely empty assistant message: the in-progress
			// generating message on the first iteration has no content and
			// no calls yet; sent to the provider it marshals as
			// content:null which OpenAI-compatible APIs reject with 400.
			if m.Content == "" && len(m.ToolCalls) == 0 {
				continue
			}
			calls := make([]provider.ToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				calls = append(calls, provider.ToolCall{
					ID:        tc.ProviderCallID,
					Name:      tc.Name,
					Arguments: tc.Arguments,
				})
			}
			out = append(out, provider.Message{Role: "assistant", Content: m.Content, Calls: calls})
		case store.RoleTool:
			out = append(out, provider.Message{
				Role:       "tool",
				Content:    m.Content,
				ToolCallID: m.ToolCallID,
			})
		}
	}
	return out, nil
}

// modelSeesImages reports whether the model's input modality includes
// images. Unknown models and metadata read failures fall back to the stored
// default (text-only), i.e. images get described.
func (e *Engine) modelSeesImages(ctx context.Context, model string) bool {
	if model == "" {
		return false
	}
	metas, err := e.cfg.ModelMetas(ctx, []string{model})
	if err != nil {
		slog.Warn("engine: load model metadata, treating model as text-only", "model", model, "error", err)
		return false
	}
	for _, m := range metas[0].InputModality {
		if m == "image" {
			return true
		}
	}
	return false
}

// imageDescription returns the cached vision-model description of att,
// generating and persisting it first when missing. ” means "no description
// available" (no describer configured, unconfigured provider/model, failed
// or empty call) and callers fall back to sending the raw image. The cached
// attachment object is updated in place so tool-loop iterations and later
// messages in the same build reuse it without re-reading the row.
func (e *Engine) imageDescription(ctx context.Context, att *store.Attachment) string {
	if att.Description != "" {
		return att.Description
	}
	if e.vision == nil {
		return ""
	}
	dctx, cancel := context.WithTimeout(ctx, describeTimeout)
	desc, err := e.vision.DescribeImage(dctx, att.Data, att.Filename)
	cancel()
	switch {
	case errors.Is(err, vision.ErrNotConfigured):
		return ""
	case errors.Is(err, context.Canceled):
		// Generation was stopped mid-description; the image build is
		// about to be abandoned anyway — no need to warn or fall back.
		return ""
	case err != nil:
		slog.Warn("engine: image description failed, sending image as-is",
			"attachment", att.ID, "filename", att.Filename, "error", err)
		return ""
	}
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return ""
	}
	if err := e.store.SetAttachmentDescription(ctx, att.ID, desc); err != nil {
		slog.Warn("engine: persist image description", "attachment", att.ID, "error", err)
		// Still usable for this generation even if persistence failed.
	}
	att.Description = desc
	att.HasDescription = true
	return desc
}

// enabledTools computes the per-chat effective tool set: config defaults
// overridden by the chat's persisted toggles (nil chat = config defaults).
// Returns display names.
func (e *Engine) enabledTools(chat *store.Chat) map[string]bool {
	out := map[string]bool{}
	for _, t := range e.catalog.Tools() {
		on := t.DefaultEnabled
		if chat != nil {
			if v, ok := chat.Tools[t.Display]; ok {
				on = v
			}
		}
		if on {
			out[t.Display] = true
		}
	}
	return out
}

// SystemPrompt returns the effective system prompt: the configured base
// prompt, trimmed. Tool definitions are NOT included here — they go out via
// the provider's native tools parameter (effectiveTools); duplicating them
// in the system prompt buys nothing for models with proper function-calling
// support.
func (e *Engine) SystemPrompt() string {
	return strings.TrimSpace(e.cfg.Get().SystemPrompt)
}

// effectiveTools returns the tool definitions to send to the provider:
// enabled tools ∪ tools referenced anywhere in the chat's history (H3 —
// omitting a disabled tool whose calls exist in history would make
// chat_completions reject the request with orphan tool_call ids).
func (e *Engine) effectiveTools(ctx context.Context, chat *store.Chat) ([]provider.Tool, error) {
	catalog := e.catalog.Tools()
	byDisplay := map[string]int{}
	for i, t := range catalog {
		byDisplay[t.Display] = i
	}
	enabled := e.enabledTools(chat)

	defs := []provider.Tool{}
	included := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(enabled)) {
		if i, ok := byDisplay[name]; ok {
			t := catalog[i]
			defs = append(defs, provider.Tool{Name: t.Display, Description: t.Description, Schema: t.Schema})
			included[t.Display] = true
		}
	}

	// H3: history-referenced tools.
	referenced, err := e.store.DistinctToolNamesInChat(ctx, chat.ID)
	if err != nil {
		return nil, err
	}
	for _, name := range referenced {
		if included[name] {
			continue
		}
		included[name] = true
		if i, ok := byDisplay[name]; ok {
			t := catalog[i]
			defs = append(defs, provider.Tool{Name: t.Display, Description: t.Description, Schema: t.Schema})
		} else {
			// Tool no longer in catalog: minimal placeholder definition so the
			// provider accepts the historical call ids.
			defs = append(defs, provider.Tool{
				Name:        name,
				Description: "(tool unavailable)",
				Schema:      json.RawMessage(`{"type":"object"}`),
			})
		}
	}
	return defs, nil
}
