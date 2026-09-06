// Package llm builds the non-streaming OpenAI-compatible clients the
// background services use (vision descriptions, title generation). One
// client is cached per live (endpoint, key, model, effort) tuple, so
// provider/model/effort changes take effect without a restart while the
// underlying http.Client — and its connection pool — survives between calls.
package llm

import (
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
// (finish_reason, usage) on their failure paths.
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

// Cache holds one client and rebuilds it when the settings it was built from
// change. One Cache per service; the mutex keeps concurrent sweeps/describes
// from racing on the rebuild.
type Cache struct {
	cfgs *config.Store

	mu  sync.Mutex
	cli *Client
	sig string // baseURL|apiKey|model|effort signature of cli
}

// NewCache builds a cache reading per-model metadata from cfgs.
func NewCache(cfgs *config.Store) *Cache { return &Cache{cfgs: cfgs} }

// Get returns the client for the model, read against the store's current
// provider settings, or nil when the model or the endpoint is not configured
// yet (callers treat that as "feature off"). The reasoning effort is the
// model's stored default from the models table; a metadata read failure falls
// back to the provider's own default rather than stalling the caller.
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
