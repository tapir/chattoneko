package engine

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"

	"chattoneko/internal/attach"
	"chattoneko/internal/provider"
	"chattoneko/internal/store"
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
// Image attachments always go out as PNG data URLs; whether the model can
// actually see them is between the user and the provider (the Composer
// warns when the picked model's metadata lacks image input).
func (e *Engine) buildProviderMessages(ctx context.Context, chat *store.Chat, msgs []*store.Message, attCache map[string]*store.Attachment) ([]provider.Message, error) {
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
				switch att.Kind {
				case attach.KindImage:
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
// reject the request with orphan tool_call ids). History-only tools get a
// bare placeholder rather than their real definition: they are declared so the
// provider accepts the old call ids, and re-advertising a disabled or removed
// tool with its full description reads to the model as an invitation to call
// it.
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
		defs = append(defs, provider.Tool{
			Name:        name,
			Description: "(tool unavailable)",
			Schema:      json.RawMessage(`{"type":"object"}`),
		})
	}
	return defs, nil
}
