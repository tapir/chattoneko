package titlegen

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// errEmptyTitle means the model returned nothing usable after sanitization;
// treated as a transient failure (retry with backoff).
var errEmptyTitle = errors.New("titlegen: model returned an empty title")

const (
	// maxInputRunes bounds how much of the first message is sent to the
	// title model — a title only needs the gist, and huge pasted files must
	// not blow up the request.
	maxInputRunes = 1000
	// maxTitleRunes caps the persisted title (sidebar labels truncate anyway;
	// this keeps pathological model output out of the DB).
	maxTitleRunes = 60
	// maxOutputTokens bounds the completion. Reasoning models (gpt-oss,
	// o-*, GLM thinking, ...) burn hidden reasoning tokens BEFORE emitting
	// content, and most providers count those against max_tokens — a tight
	// cap (e.g. 64) gets fully consumed by reasoning and the API returns
	// finish_reason=length with EMPTY content. 1024 leaves ample headroom;
	// the system prompt still keeps the visible title to a few words.
	maxOutputTokens = 1024

	systemPrompt = `You generate short titles for chat conversations. ` +
		`Reply with ONLY the title: at most 6 words, no quotation marks, ` +
		`no trailing punctuation, no "Title:" prefix.`
)

// client is the dedicated OpenAI-compatible client for title generation.
// Separate from the chat provider by design: it issues plain non-streaming
// completions with the configured task model and shares no state with the
// chat streaming machinery.
type client struct {
	api    *openai.Client
	model  string
	effort string // reasoning effort sent with every call; "" = provider default
}

// newClient builds the task client against the same provider endpoint/key as
// chat (there is exactly one provider), but with the task model and its
// stored default reasoning effort.
func newClient(baseURL, apiKey, model, effort string) *client {
	c := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(baseURL),
	)
	return &client{api: &c, model: model, effort: effort}
}

// GenerateFromText titles a conversation whose first message is typed text.
func (c *client) GenerateFromText(ctx context.Context, text string) (string, error) {
	return c.complete(ctx, "Generate title for this user message:\n\"\"\"\n"+truncateRunes(text, maxInputRunes)+"\n\"\"\"")
}

// GenerateFromFile titles a conversation whose first message is a text file
// attachment (filename included for context).
func (c *client) GenerateFromFile(ctx context.Context, filename, content string) (string, error) {
	return c.complete(ctx, fmt.Sprintf("Generate a title for this content (filename: %q):\n\"\"\"\n%s\n\"\"\"",
		filename, truncateRunes(content, maxInputRunes)))
}

// complete issues one non-streaming completion and sanitizes the result.
func (c *client) complete(ctx context.Context, userPrompt string) (string, error) {
	params := openai.ChatCompletionNewParams{
		Model: c.model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage(userPrompt),
		},
		MaxTokens: openai.Int(maxOutputTokens),
	}
	if c.effort != "" {
		params.ReasoningEffort = shared.ReasoningEffort(c.effort)
	}
	resp, err := c.api.Chat.Completions.New(ctx, params)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("titlegen: response has no choices")
	}
	title, err := sanitizeTitle(resp.Choices[0].Message.Content)
	if err != nil {
		// Attach the provider's own diagnostics: an empty title from a
		// reasoning model almost always means finish_reason=length —
		// reasoning tokens ate the whole max_tokens budget.
		ch := resp.Choices[0]
		return "", fmt.Errorf("%w (finish_reason=%s completion_tokens=%d reasoning_tokens=%d)",
			err, ch.FinishReason, resp.Usage.CompletionTokens,
			resp.Usage.CompletionTokensDetails.ReasoningTokens)
	}
	return title, nil
}

// sanitizeTitle normalizes raw model output into a persistable title: first
// line only, no surrounding quotes, no "Title:" prefix, collapsed
// whitespace, no control characters, no trailing punctuation, length-capped
// at a word boundary. The input is untrusted model output.
func sanitizeTitle(raw string) (string, error) {
	line, _, _ := strings.Cut(raw, "\n")
	s := strings.TrimSpace(line)
	// Drop a "Title:" style prefix (case-insensitive) — models love it.
	if i := strings.Index(s, ":"); i > 0 && i <= 8 {
		if strings.EqualFold(strings.TrimSpace(s[:i]), "title") {
			s = strings.TrimSpace(s[i+1:])
		}
	}
	s = strings.Trim(s, "\"'`“”‘’«»")
	s = strings.Join(strings.Fields(s), " ")
	// Model output must not smuggle control characters into the DB, SSE
	// payloads or sidebar labels.
	if strings.ContainsFunc(s, unicode.IsControl) {
		s = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, s)
	}
	s = strings.TrimRightFunc(s, func(r rune) bool {
		return unicode.IsPunct(r) && r != '(' && r != '['
	})
	if s == "" {
		return "", errEmptyTitle
	}
	return truncateRunes(s, maxTitleRunes), nil
}

// truncateRunes cuts s to at most n runes, preferring a word boundary, and
// never splitting a multi-byte rune.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	cut := string(runes[:n])
	// Prefer ending at the last space so we don't slice a word in half.
	if i := strings.LastIndex(cut, " "); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut)
}
