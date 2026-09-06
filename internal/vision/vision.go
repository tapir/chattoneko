// Package vision describes images for chat models that lack image input:
// each image attachment is run ONCE through the configured vision model and
// the resulting text description is cached on the attachment row by the
// engine. The call goes through the shared non-streaming client cache
// (internal/llm), so provider/model/effort changes apply live. Independence
// contract: this service shares no state with the chat streaming machinery —
// same pattern as titlegen.
package vision

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"unicode"

	"github.com/openai/openai-go/v3"

	"chattoneko/internal/config"
	"chattoneko/internal/llm"
)

// ErrNotConfigured means the provider and/or vision model is not set up.
// Callers treat it as "no description available" and fall back to sending
// the image itself.
var ErrNotConfigured = errors.New("vision: no vision model configured")

const (
	// maxOutputTokens bounds one description. Vision models can be
	// generous with detail; 8192 leaves room for a thorough description
	// without letting a runaway model blow up the cached text.
	maxOutputTokens = 8192

	systemPrompt = `You describe images for a model that cannot see them. ` +
		`Describe the attached image in detail: the scene, subjects, objects, ` +
		`people and their actions, any visible text, colors, layout and overall ` +
		`composition. Be factual and specific. Reply with ONLY the description, ` +
		`no preamble.`
)

// Service describes images via the configured vision model.
type Service struct {
	cfgs  *config.Store
	cache *llm.Cache
}

// New builds the service over the live config store.
func New(cfgs *config.Store) *Service {
	return &Service{cfgs: cfgs, cache: llm.NewCache(cfgs)}
}

// DescribeImage returns a text description of the PNG data. Returns
// ErrNotConfigured when the provider/vision model is not set up; provider
// errors propagate unchanged.
func (s *Service) DescribeImage(ctx context.Context, png []byte, filename string) (string, error) {
	cli := s.cache.Get(ctx, s.cfgs.Get().Models.DefaultVisionModel)
	if cli == nil {
		return "", ErrNotConfigured
	}
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	resp, err := cli.Complete(ctx, []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(systemPrompt),
		openai.UserMessage([]openai.ChatCompletionContentPartUnionParam{
			// Filenames are external input (uploads, URL-derived); keep
			// control characters out of the prompt.
			openai.TextContentPart("Describe this image (original filename: " + stripControl(filename, false) + ")."),
			openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{URL: dataURL}),
		}),
	}, maxOutputTokens)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("vision: response has no choices")
	}
	// The description is model output (untrusted): strip control characters
	// before it reaches the chat prompt, the description endpoint or the chat
	// log. Newlines/tabs stay — they are legitimate description formatting.
	return stripControl(resp.Choices[0].Message.Content, true), nil
}

// stripControl removes control characters from untrusted text. With
// keepNewlines, '\n' and '\t' survive (description body formatting);
// without it, every control character is dropped (filenames).
func stripControl(s string, keepNewlines bool) string {
	if !strings.ContainsFunc(s, unicode.IsControl) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if !unicode.IsControl(r) {
			return r
		}
		if keepNewlines && (r == '\n' || r == '\t') {
			return r
		}
		return -1
	}, s)
}
