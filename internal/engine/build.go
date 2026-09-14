package engine

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"chattoneko/internal/attach"
	"chattoneko/internal/provider"
	"chattoneko/internal/store"
	"chattoneko/internal/tools"
)

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
// vision says whether the current model's metadata lists image input: an
// image goes out as a PNG data URL when it does, and as a stored-file
// reference when it doesn't. Binary attachments (audio, PDF, tool files) have
// no wire representation at all and always take the reference path, so the
// model at least learns the file exists and what its id is.
func (e *Engine) buildProviderMessages(ctx context.Context, chat *store.Chat, msgs []*store.Message, attCache map[string]*store.Attachment, vision bool) ([]provider.Message, error) {
	out := make([]provider.Message, 0, len(msgs)+1)
	if sp := e.SystemPrompt(); sp != "" {
		out = append(out, provider.Message{Role: "system", Content: sp})
	}
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
				if att.Kind == attach.KindText {
					content += "\n\n" + attach.SerializeText(att.Filename, att.ID, string(att.Data))
				} else if att.Kind == attach.KindImage && vision && attach.SendsAsImage(att.Mime) {
					// Only PNG and WebP go out as images: a JPEG, GIF, BMP or ICO
					// a tool fetched previews in the browser but takes the
					// reference path, since a provider 400 here would repeat on
					// every turn of the chat.
					images = append(images, provider.Image{Data: att.Data, Mime: att.Mime})
				} else {
					content += "\n\n" + attach.SerializeRef(att.Filename, att.ID, att.Kind, att.Mime, att.Size)
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

// inputModalities returns the model's stored input modalities. Metadata never
// fetched from the provider defaults to text-only (config.DefaultModelMeta),
// and a failed lookup is treated the same way: the fallback costs the model a
// picture (it still gets the attachment's id) and never sends image parts to
// an endpoint that rejects them.
func (e *Engine) inputModalities(ctx context.Context, model string) []string {
	metas, err := e.cfg.ModelMetas(ctx, []string{model})
	if err != nil || len(metas) == 0 {
		slog.Warn("engine: load model metadata", "model", model, "error", err)
		return nil
	}
	return metas[0].InputModality
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
// omitting a tool whose calls exist in history would make chat_completions
// reject the request with orphan tool_call ids). mods are the chat model's
// input modalities: they decide what the agent tool offers, if anything.
// History-only tools get a bare placeholder rather than their real definition:
// they are declared so the provider accepts the old call ids, and
// re-advertising a disabled or removed tool with its full description reads to
// the model as an invitation to call it.
func (e *Engine) effectiveTools(ctx context.Context, chat *store.Chat, mods []string) ([]provider.Tool, error) {
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
			desc := t.Description
			// The agent tool speaks for the file types this chat model cannot
			// take itself; when that is none of them it stays out of the
			// request entirely.
			if t.Display == tools.AgentName {
				var needed bool
				if desc, needed = tools.AgentDescription(mods); !needed {
					continue
				}
			}
			defs = append(defs, provider.Tool{Name: t.Display, Description: desc, Schema: t.Schema})
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
		defs = append(defs, provider.Tool{
			Name:        name,
			Description: "(tool unavailable)",
			Schema:      json.RawMessage(`{"type":"object"}`),
		})
	}
	return defs, nil
}
