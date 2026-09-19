package titlegen

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"

	"chattoneko/internal/llm"
)

// errEmptyTitle means the model returned nothing usable after sanitization;
// treated as a transient failure and retried after a delay.
var errEmptyTitle = errors.New("titlegen: model returned an empty title")

const (
	// maxInputRunes caps how much of the first message reaches the title model.
	maxInputRunes = 1000
	// maxTitleRunes caps the persisted title, keeping pathological model output
	// out of the DB.
	maxTitleRunes = 60
	// maxOutputTokens bounds the completion. Reasoning models burn hidden
	// reasoning tokens before emitting content and most providers count those
	// against max_tokens, so a tight cap comes back as finish_reason=length with
	// empty content; the system prompt keeps the visible title short anyway.
	maxOutputTokens = 1024

	systemPrompt = `You generate short titles for chat conversations. ` +
		`Reply with ONLY the title: at most 6 words, no quotation marks, ` +
		`no trailing punctuation, no "Title:" prefix.`
)

// client issues title prompts through the non-streaming internal/llm client
// with the configured task model, sharing no state with the chat streaming
// machinery.
type client struct {
	llm *llm.Client
}

// GenerateFromText titles a conversation whose first message is typed text.
func (c *client) GenerateFromText(ctx context.Context, text string) (string, error) {
	return c.complete(ctx, "Generate title for this user message:\n\"\"\"\n"+truncateRunes(text, maxInputRunes)+"\n\"\"\"")
}

// GenerateFromFile titles a conversation whose first message is a text file
// attachment, including the filename as context.
func (c *client) GenerateFromFile(ctx context.Context, filename, content string) (string, error) {
	return c.complete(ctx, fmt.Sprintf("Generate a title for this content (filename: %q):\n\"\"\"\n%s\n\"\"\"",
		filename, truncateRunes(content, maxInputRunes)))
}

// complete issues one non-streaming completion and sanitizes the result.
func (c *client) complete(ctx context.Context, userPrompt string) (string, error) {
	resp, err := c.llm.Complete(ctx, []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(systemPrompt),
		openai.UserMessage(userPrompt),
	}, maxOutputTokens)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("titlegen: response has no choices")
	}
	title, err := sanitizeTitle(resp.Choices[0].Message.Content)
	if err != nil {
		// Attach the provider's diagnostics: an empty title from a reasoning
		// model almost always means reasoning tokens consumed the max_tokens
		// budget (finish_reason=length).
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
	// Drop a "Title:" style prefix (case-insensitive) — models often emit one.
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
