// Package llm builds the non-streaming OpenAI-compatible clients used off the
// chat path: title generation and the specialist attachment tools.
package llm

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"chattoneko/internal/config"
)

// Client issues plain non-streaming completions with one model.
type Client struct {
	api    *openai.Client
	model  string
	effort string // reasoning effort sent with every call; "" = provider default
}

// Complete runs one non-streaming completion and returns the raw response, so
// callers read the choice content plus the provider's own diagnostics
// (finish_reason, usage).
func (c *Client) Complete(ctx context.Context, msgs []openai.ChatCompletionMessageParamUnion, maxTokens int64) (*openai.ChatCompletion, error) {
	params := openai.ChatCompletionNewParams{
		Model:     c.model,
		Messages:  msgs,
		MaxTokens: openai.Int(maxTokens),
	}
	if c.effort != "" {
		params.ReasoningEffort = shared.ReasoningEffort(c.effort)
	}
	return c.api.Chat.Completions.New(ctx, params)
}

// Transcribe sends audio to /audio/transcriptions and returns the text. The
// endpoint identifies the container from the multipart filename and mime,
// not from the bytes.
func (c *Client) Transcribe(ctx context.Context, filename, mime string, data []byte) (string, error) {
	resp, err := c.api.Audio.Transcriptions.New(ctx, openai.AudioTranscriptionNewParams{
		Model: openai.AudioModel(c.model),
		File:  openai.File(bytes.NewReader(data), filename, mime),
	})
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}

// Cache holds one client built from a config.Store's provider settings and
// per-model metadata, and rebuilds it when any of them change — settings take
// effect without a restart while the http.Client and its connection pool
// survive between calls that resolve to the same client. Callers hold one
// Cache each; the mutex keeps concurrent Gets from racing on the rebuild.
type Cache struct {
	cfgs *config.Store

	mu  sync.Mutex
	cli *Client
	sig string // baseURL|apiKey|model|effort signature of cli
}

func NewCache(cfgs *config.Store) *Cache { return &Cache{cfgs: cfgs} }

// Get returns the client for model under the store's current provider
// settings, or nil when the model or the endpoint is unset — callers treat
// that as "feature off". The effort is the model's stored default; a metadata
// read failure leaves it empty so the provider's own default applies rather
// than stalling the caller.
func (c *Cache) Get(ctx context.Context, model string) *Client {
	if model == "" {
		return nil
	}
	p := c.cfgs.Get().Provider
	if p.BaseURL == "" || p.APIKey == "" {
		return nil
	}
	effort := ""
	metas, err := c.cfgs.ModelMetas(ctx, []string{model})
	if err != nil {
		slog.Warn("llm: load model metadata", "model", model, "error", err)
	} else {
		effort = metas[0].ReasoningDefault
	}
	sig := strings.Join([]string{p.BaseURL, p.APIKey, model, effort}, "\x00")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cli == nil || c.sig != sig {
		api := openai.NewClient(option.WithAPIKey(p.APIKey), option.WithBaseURL(p.BaseURL))
		c.cli = &Client{api: &api, model: model, effort: effort}
		c.sig = sig
	}
	return c.cli
}
