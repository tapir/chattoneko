// Package vision describes images for chat models that lack image input:
// each image attachment is run ONCE through the configured vision model
// (a dedicated non-streaming client against the same provider endpoint as
// chat) and the resulting text description is cached on the attachment row
// by the engine. Independence contract: this client shares no state with the
// chat streaming machinery — same pattern as titlegen's task client.
package vision

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"unicode"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"chattoneko/internal/config"
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

// Service describes images via the configured vision model. The client is
// rebuilt lazily whenever the provider or vision-model config changes (or
// the model's default reasoning effort), so config edits take effect live
// without a restart.
type Service struct {
	cfgs *config.Store

	mu  sync.Mutex
	cli *client
	sig string // baseURL|apiKey|model signature of cli
}

// New builds the service over the live config store.
func New(cfgs *config.Store) *Service { return &Service{cfgs: cfgs} }

// DescribeImage returns a text description of the PNG data. Returns
// ErrNotConfigured when the provider/vision model is not set up; provider
// errors propagate unchanged.
func (s *Service) DescribeImage(ctx context.Context, png []byte, filename string) (string, error) {
	cli := s.client(ctx)
	if cli == nil {
		return "", ErrNotConfigured
	}
	return cli.describe(ctx, png, filename)
}

// client caches a client for the current (baseURL, apiKey, model, effort)
// tuple and rebuilds it when any of them changes. The reasoning effort is
// the vision model's default from the models table; a metadata read failure
// falls back to the provider's own default rather than stalling
// descriptions.
func (s *Service) client(ctx context.Context) *client {
	c := s.cfgs.Get()
	if c.Provider.BaseURL == "" || c.Provider.APIKey == "" || c.Models.DefaultVisionModel == "" {
		return nil
	}
	effort := ""
	metas, err := s.cfgs.ModelMetas(ctx, []string{c.Models.DefaultVisionModel})
	if err != nil {
		slog.Warn("vision: load model metadata", "model", c.Models.DefaultVisionModel, "error", err)
	} else {
		effort = metas[0].ReasoningDefault
	}
	sig := c.Provider.BaseURL + "\x00" + c.Provider.APIKey + "\x00" + c.Models.DefaultVisionModel + "\x00" + effort
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cli == nil || s.sig != sig {
		s.cli = newClient(c.Provider.BaseURL, c.Provider.APIKey, c.Models.DefaultVisionModel, effort)
		s.sig = sig
	}
	return s.cli
}

// client issues plain non-streaming completions with the vision model.
type client struct {
	api    *openai.Client
	model  string
	effort string // reasoning effort sent with every call; "" = provider default
}

func newClient(baseURL, apiKey, model, effort string) *client {
	c := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(baseURL),
	)
	return &client{api: &c, model: model, effort: effort}
}

// describe runs one bounded completion: the image as a data URL plus the
// filename as a hint, answered with a text-only description.
func (c *client) describe(ctx context.Context, png []byte, filename string) (string, error) {
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	params := openai.ChatCompletionNewParams{
		Model: c.model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage([]openai.ChatCompletionContentPartUnionParam{
				// Filenames are external input (uploads, URL-derived); keep
				// control characters out of the prompt.
				openai.TextContentPart("Describe this image (original filename: " + stripControl(filename, false) + ")."),
				openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{URL: dataURL}),
			}),
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
