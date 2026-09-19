// Package titlegen runs the background title-generation task: it polls for
// chats whose title is not final yet (title_generated = 0), derives a title
// from the first user message through a dedicated non-chat OpenAI-compatible
// client, and hands the result to a publish callback (the engine's global SSE
// stream).
//
// The polling loop and the LLM client share no locks, channels or buffers with
// the engine's generation machinery, so a stalled chat generation can never
// delay a title. The store's conditional write (SetGeneratedTitle) lets a
// concurrent manual rename win over the task.
package titlegen

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"chattoneko/internal/attach"
	"chattoneko/internal/config"
	"chattoneko/internal/llm"
	"chattoneko/internal/store"
)

// generator produces a title from the first-message source; client.go
// implements it, tests fake it.
type generator interface {
	GenerateFromText(ctx context.Context, text string) (string, error)
	GenerateFromFile(ctx context.Context, filename, content string) (string, error)
}

const (
	// defaultInterval is the poll cadence. Set in code, not a runtime setting;
	// tests shrink it.
	defaultInterval = 1 * time.Second
	// defaultTimeout bounds one title call so a hung provider stalls at most
	// one sweep, not the whole task.
	defaultTimeout = 30 * time.Second
	// defaultBatch caps chats processed per sweep (only brand-new chats ever
	// qualify, so this is headroom, not a rate limiter).
	defaultBatch = 16
	// maxFailures bounds retries per chat on transient errors (provider outage,
	// DB hiccup); afterwards the chat keeps "New Chat" and the task stops
	// hammering a broken setup.
	maxFailures = 5
	// retryDelay is the fixed pause between two attempts for one chat, so a
	// provider blip is retried a few times over ~a minute instead of every sweep.
	retryDelay = 10 * time.Second

	// imageOnlyTitle is the fixed title for chats whose first message is
	// image-only (no text to summarize).
	imageOnlyTitle = "User Image Input"
)

// retryState tracks transient failures for one chat. Only the sweep goroutine
// touches the map (processing is serial), so no lock is needed.
type retryState struct {
	failures int
	next     time.Time // earliest retry time
}

// Service is the background title-generation task and the only writer of
// auto-generated titles.
type Service struct {
	store *store.Store
	cfgs  *config.Store
	gen   generator // test override; nil in production
	// publish announces a final title (engine.PublishTitle in production).
	publish func(chatID, title string)

	interval time.Duration
	timeout  time.Duration
	batch    int64

	retries map[string]*retryState

	// cache holds the live task client (endpoint/key/model/effort).
	cache *llm.Cache
}

// New builds the task. The title client (same endpoint/key as the chat
// provider, task model from config) is created lazily from the live config, so
// provider and model changes apply without a restart.
func New(st *store.Store, cfgs *config.Store, publish func(chatID, title string)) *Service {
	return &Service{
		store:    st,
		cfgs:     cfgs,
		publish:  publish,
		interval: defaultInterval,
		timeout:  defaultTimeout,
		batch:    defaultBatch,
		retries:  map[string]*retryState{},
		cache:    llm.NewCache(cfgs),
	}
}

// generator returns the active title generator: the test override if set,
// otherwise the live task client built from the current config. Returns nil
// when the provider/task model is not configured yet (sweeps skip until it is).
// The explicit nil check matters: a nil *client wrapped in the generator
// interface would be non-nil and panic on first use.
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

// taskClient returns the title client for the current config (task model +
// its stored default reasoning effort), or nil when the provider/task model
// is not configured yet — see internal/llm for the caching.
func (s *Service) taskClient(ctx context.Context) *client {
	cli := s.cache.Get(ctx, s.cfgs.Get().Models.DefaultTaskModel)
	if cli == nil {
		return nil
	}
	return &client{llm: cli}
}

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
	// Drop the retry state of chats that left the candidate list while backing
	// off (deleted, manually titled, or marked final). At the batch cap the list
	// may be truncated, so a chat beyond the window loses its state and is simply
	// retried on the next sweep instead of after the delay.
	for id := range s.retries {
		if !slices.Contains(ids, id) {
			delete(s.retries, id)
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

// generateFromAttachment titles a chat from a text attachment's content.
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
// conditional write reports 0 rows, a manual rename (or chat deletion) won the
// race: the user's title stays and nothing is broadcast.
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
	s.publish(chatID, title)
}

// fail records a transient failure and schedules the retry after retryDelay;
// after maxFailures it gives up, keeping "New Chat" and marking the title
// final.
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
	r.next = time.Now().Add(retryDelay)
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
