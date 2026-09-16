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

// defaultStreamIdleTimeout bounds how long a stream may stay silent; nothing
// else aborts an in-flight stream. Generous enough for slow first-token
// providers.
const defaultStreamIdleTimeout = 5 * time.Minute

// chatCompletionsProvider implements Provider over POST /chat/completions.
type chatCompletionsProvider struct {
	client *openai.Client
	// idleTimeout aborts a stream that delivers no data for this long; tests
	// shrink it.
	idleTimeout time.Duration
}

var _ Provider = (*chatCompletionsProvider)(nil)

func newChatCompletionsProvider(baseURL, apiKey string) *chatCompletionsProvider {
	client := openai.NewClient(option.WithAPIKey(apiKey), option.WithBaseURL(baseURL))
	return &chatCompletionsProvider{client: &client, idleTimeout: defaultStreamIdleTimeout}
}

func dataURL(img Image) string {
	return "data:" + img.Mime + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
}

// functionTools converts normalized tool definitions to the SDK shape.
// An invalid JSON schema is a hard error: substituting a generic schema
// would hide misconfigured tools.
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
			// Image parts first, the text last: some OpenAI-compatible vision
			// routes (OpenRouter deepseek/deepseek-v4-flash-vision-exp) silently
			// drop a text part that precedes the image, so the model answers as if
			// no question was asked. Text after the images is accepted everywhere.
			parts := make([]openai.ChatCompletionContentPartUnionParam, 0, len(m.Images)+1)
			for _, img := range m.Images {
				parts = append(parts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
					URL: dataURL(img),
				}))
			}
			if m.Content != "" {
				parts = append(parts, openai.TextContentPart(m.Content))
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

// buildCompletionParams maps GenParams to the SDK request params; StreamChat
// fills in messages and tools.
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
	// include_usage makes the provider end the stream with a usage chunk.
	opts = append(opts, option.WithJSONSet("stream_options", map[string]any{"include_usage": true}))

	streamCtx, cancel := context.WithCancel(ctx)
	es := NewEventStream(256, cancel)

	go func() {
		defer cancel()
		// Idle watchdog: without it a half-open connection holds the generation
		// open until someone stops it by hand.
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
			args      strings.Builder // argument fragments arrive as many small appends
			startSent bool
		}
		toolAcc := map[int64]*toolState{}
		finish := ""
		var usage Usage
		for stream.Next() {
			chunk := stream.Current()
			idle.Reset(c.idleTimeout)
			// The terminal usage chunk arrives with no choices.
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
			// Reasoning arrives as an undocumented field: OpenRouter uses
			// "reasoning", others (e.g. DeepSeek direct) use "reasoning_content".
			// The SDK stores unknown fields in JSON.ExtraFields with valid=false,
			// so read Raw() rather than Valid().
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
				// Start fires once both id and name are known: some providers
				// stream the id after the name, and an empty call id is useless
				// to the client. The done event carries both anyway.
				if !acc.startSent && acc.id != "" && acc.name != "" {
					acc.startSent = true
					if !es.Publish(StreamEvent{Kind: EventToolCallStart, CallID: acc.id, Name: acc.name}) {
						return
					}
				}
				if tc.Function.Arguments != "" {
					acc.args.WriteString(tc.Function.Arguments)
					// Forward argument fragments so the UI can show them streaming.
					// Fragments seen before the start event are only accumulated:
					// the client could not attribute them to a call, and the done
					// event carries the complete arguments. Some providers (e.g.
					// OpenRouter for Kimi/GLM) send the whole call in one chunk at
					// the end of the stream, where there is nothing to stream.
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
				// The cancel came from the watchdog, not a user stop: report the
				// idle cause instead of a bare context error.
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
				// reference calls by tool_call_id) and would poison every request
				// replaying that history, so drop it loudly.
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
