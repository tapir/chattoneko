// Package titlegen runs the background title-generation task: it polls for
// chats whose title is not final yet (title_generated = 0), derives a title
// from the first user message via a dedicated (non-chat) OpenAI-compatible
// client, and publishes the result on its own independent SSE fan-out.
//
// Independence contract: the Hub, the polling loop and the LLM client share
// NO locks, channels or buffers with the engine's generation machinery — a
// stalled chat generation or a slow per-chat SSE subscriber can never delay
// or drop a title update. The store's conditional write
// (SetGeneratedTitle) makes a concurrent manual rename win over the task.
package titlegen

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"chattoneko/internal/attach"
	"chattoneko/internal/config"
	"chattoneko/internal/store"
)

// generator produces a title from the first-message source. The production
// implementation is the dedicated OpenAI client (client.go); tests fake it.
type generator interface {
	GenerateFromText(ctx context.Context, text string) (string, error)
	GenerateFromFile(ctx context.Context, filename, content string) (string, error)
}

const (
	// defaultInterval is the poll cadence. Code-configurable (not a runtime
	// setting): tests shrink it, production keeps the default.
	defaultInterval = 1 * time.Second
	// defaultTimeout bounds one title call so a hung provider stalls at most
	// one sweep, not the whole task.
	defaultTimeout = 30 * time.Second
	// defaultBatch caps chats processed per sweep (only brand-new chats ever
	// qualify, so this is headroom, not a rate limiter).
	defaultBatch = 16
	// maxFailures bounds retries per chat on transient errors (provider
	// outage, DB hiccup); afterwards the chat keeps "New Chat" and the task
	// stops hammering a broken setup.
	maxFailures = 5

	// imageOnlyTitle is the fixed title for chats whose first message is
	// image-only (no text to summarize).
	imageOnlyTitle = "User Image Input"
)

// retryState tracks transient failures for one chat. Only the sweep
// goroutine touches the map (processing is serial), so no lock is needed.
type retryState struct {
	failures int
	next     time.Time // do not retry before this time
}

// backoff returns the delay before the next attempt after n failures.
func backoff(failures int) time.Duration {
	d := time.Duration(1<<failures) * time.Second // 2s, 4s, 8s, 16s, 32s
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// Service is the background title-generation task. It is the ONLY component
// allowed to write auto-generated titles.
type Service struct {
	store *store.Store
	cfgs  *config.Store
	gen   generator // test override; nil in production
	hub   *Hub

	interval time.Duration
	timeout  time.Duration
	batch    int64

	retries map[string]*retryState

	cliMu  sync.Mutex
	cli    *client
	cliSig string // baseURL|apiKey|model signature of cli
}

// New builds the task. The title client (separate from the chat provider;
// same endpoint/key, task model from config) is created lazily from the live
// config, so provider/model changes take effect without a restart.
func New(st *store.Store, cfgs *config.Store) *Service {
	return &Service{
		store:    st,
		cfgs:     cfgs,
		hub:      NewHub(),
		interval: defaultInterval,
		timeout:  defaultTimeout,
		batch:    defaultBatch,
		retries:  map[string]*retryState{},
	}
}

// generator returns the active title generator: the test override if set,
// otherwise the live task client built from the current config. Returns nil
// when the provider/task model is not configured yet (sweeps skip until it
// is). Note the explicit nil handling: a nil *client wrapped in the
// generator interface would be non-nil and panic on first use.
func (s *Service) generator(ctx context.Context) generator {
	if s.gen != nil {
		return s.gen
	}
	cli := s.taskClient(ctx)
	if cli == nil {
		return nil
	}
	return cli
}

// taskClient caches a client for the current (baseURL, apiKey, model, effort)
// tuple and rebuilds it when any of them changes. The reasoning effort is
// the task model's default from the models table; a metadata read failure
// falls back to the provider's own default rather than stalling titles.
func (s *Service) taskClient(ctx context.Context) *client {
	c := s.cfgs.Get()
	if c.Provider.BaseURL == "" || c.Provider.APIKey == "" || c.Models.DefaultTaskModel == "" {
		return nil
	}
	effort := ""
	metas, err := s.cfgs.ModelMetas(ctx, []string{c.Models.DefaultTaskModel})
	if err != nil {
		slog.Warn("title task: load model metadata", "model", c.Models.DefaultTaskModel, "error", err)
	} else {
		effort = metas[0].ReasoningDefault
	}
	sig := c.Provider.BaseURL + "\x00" + c.Provider.APIKey + "\x00" + c.Models.DefaultTaskModel + "\x00" + effort
	s.cliMu.Lock()
	defer s.cliMu.Unlock()
	if s.cli == nil || s.cliSig != sig {
		s.cli = newClient(c.Provider.BaseURL, c.Provider.APIKey, c.Models.DefaultTaskModel, effort)
		s.cliSig = sig
	}
	return s.cli
}

// Hub returns the title-event fan-out (for the SSE endpoint).
func (s *Service) Hub() *Hub { return s.hub }

// Run polls until ctx is cancelled. Serial: one sweep at a time, one chat at
// a time — a slow provider delays later chats by at most timeout each, and
// duplicate title calls for one chat are impossible by construction.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep(ctx)
		}
	}
}

// sweep processes one batch of chats whose title is not final yet.
func (s *Service) sweep(ctx context.Context) {
	ids, err := s.store.ListChatsNeedingTitle(ctx, s.batch)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			slog.Warn("title task: list candidates", "error", err)
		}
		return
	}
	// Prune retry state of chats that left the candidate list (deleted,
	// renamed, or marked final) while backing off. Only safe when the
	// candidate list is complete: at the batch cap a missing id might just
	// sit beyond the window.
	if len(ids) < int(s.batch) && len(s.retries) > 0 {
		candidates := make(map[string]bool, len(ids))
		for _, id := range ids {
			candidates[id] = true
		}
		for id := range s.retries {
			if !candidates[id] {
				delete(s.retries, id)
			}
		}
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		s.process(ctx, id)
	}
}

// process decides one chat's title from its first user message:
// typed text → AI title from the text; text file → AI title from the file
// content; image-only → fixed "User Image Input"; nothing usable → keep
// "New Chat" and stop retrying.
func (s *Service) process(ctx context.Context, chatID string) {
	if r, ok := s.retries[chatID]; ok && time.Now().Before(r.next) {
		return // backing off after a transient failure
	}
	msg, err := s.store.FirstUserMessage(ctx, chatID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return // no user message yet — wait for the first one
	case err != nil:
		s.fail(ctx, chatID, "load first message", err)
		return
	}
	gen := s.generator(ctx)
	if gen == nil {
		return // provider/task model not configured yet; retry on a later sweep
	}
	if text := strings.TrimSpace(msg.Content); text != "" {
		s.generate(ctx, chatID, func(c context.Context) (string, error) {
			return gen.GenerateFromText(c, text)
		})
		return
	}
	atts, err := s.store.ListAttachmentsByMessage(ctx, msg.ID)
	if err != nil {
		s.fail(ctx, chatID, "list attachments", err)
		return
	}
	for _, a := range atts {
		if a.Kind == attach.KindText {
			s.generateFromAttachment(ctx, gen, chatID, a.ID, a.Filename)
			return
		}
	}
	for _, a := range atts {
		if a.Kind == attach.KindImage {
			s.applyTitle(ctx, chatID, imageOnlyTitle)
			return
		}
	}
	// First message carries nothing usable (empty text, no attachments):
	// keep "New Chat" and mark the title final — retrying would never help.
	s.markFinal(ctx, chatID)
}

// generateFromAttachment loads a text attachment and titles from its
// content. gen is the generator process already resolved.
func (s *Service) generateFromAttachment(ctx context.Context, gen generator, chatID, attachmentID, filename string) {
	att, err := s.store.GetAttachment(ctx, attachmentID)
	if err != nil {
		s.fail(ctx, chatID, "load attachment", err)
		return
	}
	s.generate(ctx, chatID, func(c context.Context) (string, error) {
		return gen.GenerateFromFile(c, filename, string(att.Data))
	})
}

// generate runs one bounded title call; success and transient failure are
// routed to their handlers.
func (s *Service) generate(ctx context.Context, chatID string, call func(context.Context) (string, error)) {
	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	title, err := call(cctx)
	if err != nil {
		s.fail(ctx, chatID, "generate title", err)
		return
	}
	s.applyTitle(ctx, chatID, title)
}

// applyTitle persists the title conditionally and broadcasts it. When the
// conditional write reports 0 rows, a manual rename (or chat deletion) won
// the race — the user's title stays and nothing is broadcast.
func (s *Service) applyTitle(ctx context.Context, chatID, title string) {
	ok, err := s.store.SetGeneratedTitle(ctx, chatID, title)
	if err != nil {
		s.fail(ctx, chatID, "persist title", err)
		return
	}
	delete(s.retries, chatID)
	if !ok {
		return
	}
	s.hub.Publish(chatID, title)
}

// fail records a transient failure with backoff; after maxFailures it gives
// up, keeping "New Chat" and marking the title final.
func (s *Service) fail(ctx context.Context, chatID, op string, err error) {
	r := s.retries[chatID]
	if r == nil {
		r = &retryState{}
		s.retries[chatID] = r
	}
	r.failures++
	if r.failures >= maxFailures {
		slog.Warn("title task: giving up, keeping default title",
			"chat", chatID, "attempts", r.failures, "last_op", op, "last_error", err)
		s.markFinal(ctx, chatID)
		return
	}
	r.next = time.Now().Add(backoff(r.failures))
	slog.Warn("title task: "+op+" failed, will retry",
		"chat", chatID, "attempt", r.failures, "error", err)
}

// markFinal flags the title as final without changing it (keeps "New Chat").
func (s *Service) markFinal(ctx context.Context, chatID string) {
	if err := s.store.MarkTitleGenerated(ctx, chatID); err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("title task: mark title final", "chat", chatID, "error", err)
	}
	delete(s.retries, chatID)
}
