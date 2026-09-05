package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// defaultStreamIdleTimeout bounds how long the stream may stay silent: a
// provider that accepted the request but sends nothing at all would
// otherwise keep the generation open forever (nothing else aborts an
// in-flight stream). Generous: slow first-token providers still emit
// reasoning/heartbeat data well within it.
const defaultStreamIdleTimeout = 5 * time.Minute

// chatCompletionsProvider implements Provider over POST /chat/completions.
type chatCompletionsProvider struct {
	client *openai.Client
	// idleTimeout aborts a stream that delivers no data at all for this
	// long; tests shrink it.
	idleTimeout time.Duration
}

var _ Provider = (*chatCompletionsProvider)(nil)

func newChatCompletionsProvider(baseURL, apiKey string) *chatCompletionsProvider {
	client := openai.NewClient(option.WithAPIKey(apiKey), option.WithBaseURL(baseURL))
	return &chatCompletionsProvider{client: &client, idleTimeout: defaultStreamIdleTimeout}
}

// dataURL encodes PNG bytes as a data URL. The MIME type is fixed to
// image/png by contract: the engine guarantees PNG (see Image).
func dataURL(data []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
}

// functionTools converts normalized tool definitions to the SDK shape. An
// invalid JSON schema is a hard error: silently substituting a generic
// schema would hide misconfigured tools and confuse the model.
func functionTools(tools []Tool) ([]openai.ChatCompletionToolUnionParam, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))
	for _, t := range tools {
		var schema openai.FunctionParameters
		if len(t.Schema) > 0 {
			if err := json.Unmarshal(t.Schema, &schema); err != nil {
				return nil, fmt.Errorf("tool %q: invalid JSON schema: %w", t.Name, err)
			}
		}
		out = append(out, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        t.Name,
			Description: openai.String(t.Description),
			Parameters:  schema,
		}))
	}
	return out, nil
}

func buildChatMessages(msgs []Message) ([]openai.ChatCompletionMessageParamUnion, error) {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "system":
			out = append(out, openai.SystemMessage(m.Content))
		case "user":
			if len(m.Images) == 0 {
				out = append(out, openai.UserMessage(m.Content))
				continue
			}
			parts := make([]openai.ChatCompletionContentPartUnionParam, 0, len(m.Images)+1)
			if m.Content != "" {
				parts = append(parts, openai.TextContentPart(m.Content))
			}
			for _, img := range m.Images {
				parts = append(parts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
					URL: dataURL(img.Data),
				}))
			}
			out = append(out, openai.UserMessage(parts))
		case "assistant":
			msg := openai.ChatCompletionAssistantMessageParam{}
			if len(m.Calls) > 0 {
				calls := make([]openai.ChatCompletionMessageToolCallUnionParam, 0, len(m.Calls))
				for _, c := range m.Calls {
					calls = append(calls, openai.ChatCompletionMessageToolCallUnionParam{
						OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
							ID: c.ID,
							Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
								Name:      c.Name,
								Arguments: c.Arguments,
							},
						},
					})
				}
				msg.ToolCalls = calls
			}
			if m.Content != "" {
				msg.Content = openai.ChatCompletionAssistantMessageParamContentUnion{
					OfString: openai.String(m.Content),
				}
			}
			out = append(out, openai.ChatCompletionMessageParamUnion{OfAssistant: &msg})
		case "tool":
			out = append(out, openai.ToolMessage(m.Content, m.ToolCallID))
		default:
			return nil, fmt.Errorf("unknown message role %q", m.Role)
		}
	}
	return out, nil
}

// buildCompletionParams maps GenParams to the SDK request params (everything
// except messages/tools, which StreamChat assembles).
func buildCompletionParams(p GenParams) openai.ChatCompletionNewParams {
	out := openai.ChatCompletionNewParams{Model: p.Model}
	if p.ReasoningEffort != "" {
		out.ReasoningEffort = shared.ReasoningEffort(p.ReasoningEffort)
	}
	return out
}

func (c *chatCompletionsProvider) StreamChat(ctx context.Context, msgs []Message, tools []Tool, p GenParams) (*EventStream, error) {
	if p.Model == "" {
		return nil, errors.New("provider: model is required")
	}
	wire, err := buildChatMessages(msgs)
	if err != nil {
		return nil, err
	}
	wireTools, err := functionTools(tools)
	if err != nil {
		return nil, err
	}
	params := buildCompletionParams(p)
	params.Messages = wire
	params.Tools = wireTools
	opts := []option.RequestOption{}
	// Ask for a terminal usage chunk so we can record per-turn token counts.
	opts = append(opts, option.WithJSONSet("stream_options", map[string]any{"include_usage": true}))

	streamCtx, cancel := context.WithCancel(ctx)
	es := NewEventStream(256, cancel)

	go func() {
		defer cancel()
		// Idle watchdog: abort when the provider goes completely silent.
		// Without it a half-open connection keeps the generation open until
		// someone stops it by hand.
		var idleTimedOut atomic.Bool
		idle := time.AfterFunc(c.idleTimeout, func() {
			idleTimedOut.Store(true)
			cancel()
		})
		defer idle.Stop()

		stream := c.client.Chat.Completions.NewStreaming(streamCtx, params, opts...)
		defer func() { _ = stream.Close() }()

		// Tool-call accumulation state, keyed by the wire's tool_calls index.
		type toolState struct {
			id, name  string
			args      strings.Builder // Builder: argument fragments arrive as O(n) appends
			startSent bool
		}
		toolAcc := map[int64]*toolState{}
		finish := ""
		var usage Usage
		for stream.Next() {
			chunk := stream.Current()
			idle.Reset(c.idleTimeout)
			// The terminal usage chunk (include_usage) arrives with no choices.
			if u := chunk.Usage; u.PromptTokens > 0 || u.CompletionTokens > 0 {
				usage = Usage{
					PromptTokens:     u.PromptTokens,
					CompletionTokens: u.CompletionTokens,
				}
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			choice := chunk.Choices[0]
			d := choice.Delta
			if d.Content != "" {
				if !es.Publish(StreamEvent{Kind: EventTextDelta, Text: d.Content}) {
					return
				}
			}
			// Reasoning arrives as an undocumented field on OpenAI-compatible
			// providers: OpenRouter uses "reasoning", others (e.g. DeepSeek
			// direct) use "reasoning_content". The SDK keeps unknown fields in
			// JSON.ExtraFields stored with valid=false (the SDK cannot type
			// them), so read Raw() directly rather than Valid().
			for _, key := range []string{"reasoning", "reasoning_content"} {
				if f, ok := d.JSON.ExtraFields[key]; ok {
					raw := f.Raw()
					if raw != "" && raw != "null" {
						var s string
						if json.Unmarshal([]byte(raw), &s) == nil && s != "" {
							if !es.Publish(StreamEvent{Kind: EventReasoningDelta, Text: s}) {
								return
							}
							break
						}
					}
				}
			}
			for _, tc := range d.ToolCalls {
				acc, seen := toolAcc[tc.Index]
				if !seen {
					acc = &toolState{}
					toolAcc[tc.Index] = acc
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				// Publish the start event as soon as BOTH id and name are known —
				// some providers stream the id later than the name, and a start
				// event with an empty call id is useless to the client (the done
				// event always carries both, so nothing is lost by waiting).
				if !acc.startSent && acc.id != "" && acc.name != "" {
					acc.startSent = true
					if !es.Publish(StreamEvent{Kind: EventToolCallStart, CallID: acc.id, Name: acc.name}) {
						return
					}
				}
				if tc.Function.Arguments != "" {
					acc.args.WriteString(tc.Function.Arguments)
					// Forward argument fragments as they arrive so the UI can show
					// the call's arguments streaming. Fragments seen before the
					// start event (id/name not both known yet) are only
					// accumulated: the client couldn't attribute them to a call,
					// and the done event re-delivers the complete arguments.
					// NOTE: providers like OpenRouter for Kimi/GLM deliver the
					// whole tool call in one chunk at the end of the stream —
					// nothing to stream there; deltas only help where the wire
					// carries them (OpenAI, DeepSeek, Gemini, ...).
					if acc.startSent {
						if !es.Publish(StreamEvent{Kind: EventToolCallDelta, CallID: acc.id, Args: tc.Function.Arguments}) {
							return
						}
					}
				}
			}
			if choice.FinishReason != "" {
				finish = string(choice.FinishReason)
			}
		}
		if err := stream.Err(); err != nil {
			if idleTimedOut.Load() {
				// The cancel above came from the watchdog, not a user stop:
				// report the real cause so the generation fails instead of
				// being misattributed.
				err = fmt.Errorf("provider stream went %v without any data", c.idleTimeout)
			}
			es.Publish(StreamEvent{Kind: EventError, Err: err})
			es.Finish(err)
			return
		}
		{
			idxs := make([]int64, 0, len(toolAcc))
			for i := range toolAcc {
				idxs = append(idxs, i)
			}
			slices.Sort(idxs)
			for _, i := range idxs {
				acc := toolAcc[i]
				// A call without an id can never be answered (tool messages
				// reference calls by tool_call_id): emitting it would persist
				// an empty id into history, and every later request replaying
				// that history would be rejected by the provider. Drop it
				// loudly instead.
				if acc.id == "" {
					slog.Warn("provider: dropping tool call without id", "index", i, "name", acc.name)
					continue
				}
				if !es.Publish(StreamEvent{Kind: EventToolCallDone, CallID: acc.id, Name: acc.name, Args: acc.args.String()}) {
					break
				}
			}
		}
		if finish == "" && len(toolAcc) > 0 {
			finish = "tool_calls"
		}
		es.Publish(StreamEvent{Kind: EventDone, Finish: finish, Usage: usage})
		es.Finish(nil)
	}()

	return es, nil
}
