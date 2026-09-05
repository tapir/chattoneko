package engine

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"chattoneko/internal/config"
	"chattoneko/internal/mcphub"
	"chattoneko/internal/provider"
	"chattoneko/internal/store"
)

// ErrGenerationActive is returned when a chat already has a generation
// running (or reserved). Handlers map it to HTTP 409.
var ErrGenerationActive = errors.New("a generation is already active for this chat")

// ToolCatalog is the aggregated tool source the engine runs against:
// integrated tools (internal/tools) merged with MCP tools (internal/mcphub).
type ToolCatalog interface {
	Tools() []mcphub.Entry
	Call(ctx context.Context, display, argsJSON string, meta mcphub.CallMeta) (string, bool, error)
}

// ImageDescriber produces text descriptions of image attachments for chat
// models that lack image input (see build.go). nil disables the feature:
// images are always sent as-is.
type ImageDescriber interface {
	DescribeImage(ctx context.Context, png []byte, filename string) (string, error)
}

// Engine runs generations. Its context is the server context: generations
// survive HTTP client disconnects and end only via stop, chat deletion, or
// server shutdown (B6).
type Engine struct {
	store     *store.Store
	prov      provider.Provider
	catalog   ToolCatalog
	cfg       *config.Store
	vision    ImageDescriber
	serverCtx context.Context

	mu   sync.Mutex
	hubs map[string]*chatHub
	wg   sync.WaitGroup // in-flight runGeneration goroutines (Shutdown waits on them)

	// Global (all-chats) stream subscribers. Separate mutex: deliverGlobal
	// runs while a hub lock is held, and the established lock order is
	// e.mu -> h.mu — taking e.mu there would invert it and deadlock.
	gmu   sync.Mutex
	gsubs map[int]chan WireEvent
	nextG int

	// flushInterval is how often streamed content is persisted mid-generation.
	// Settable before starting generations (tests shrink it).
	flushInterval time.Duration

	// graceInterval is how long a finished generation's replay buffer is
	// kept for late subscribers. Settable before starting generations (tests
	// shrink it).
	graceInterval time.Duration
}

// New creates the engine. vision may be nil (no image descriptions: images
// are always sent to the chat model as-is).
func New(serverCtx context.Context, st *store.Store, prov provider.Provider, catalog ToolCatalog, cfg *config.Store, vision ImageDescriber) *Engine {
	return &Engine{
		store:         st,
		prov:          prov,
		catalog:       catalog,
		cfg:           cfg,
		vision:        vision,
		serverCtx:     serverCtx,
		hubs:          map[string]*chatHub{},
		gsubs:         map[int]chan WireEvent{},
		flushInterval: defaultFlushInterval,
		graceInterval: defaultGracePeriod,
	}
}

// ModelInfo returns per-model metadata (context window, modalities, reasoning
// efforts) for the current whitelist. Reads the models table live; ids
// without a stored row get the config defaults.
func (e *Engine) ModelInfo(ctx context.Context) []config.ModelMeta {
	metas, err := e.cfg.ModelMetas(ctx, e.cfg.Get().Models.Whitelist)
	if err != nil {
		slog.Warn("engine: load model metadata", "error", err)
		return nil
	}
	return metas
}

func (e *Engine) hubFor(chatID string) *chatHub {
	e.mu.Lock()
	defer e.mu.Unlock()
	h, ok := e.hubs[chatID]
	if !ok {
		h = newChatHub(chatID, e.deliverGlobal)
		e.hubs[chatID] = h
	}
	return h
}

// dropHub removes a chat's hub (chat deletion).
func (e *Engine) dropHub(chatID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.hubs, chatID)
}

// hubIfExists returns the chat's hub or nil (never creates one).
func (e *Engine) hubIfExists(chatID string) *chatHub {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hubs[chatID]
}

// maybePruneHub drops the chat's hub when nothing references it anymore: no
// subscribers, no generation (a finalized generation in its replay grace
// period still counts) and no pending claim. Keeps engine.hubs bounded on
// long-running servers.
func (e *Engine) maybePruneHub(chatID string, h *chatHub) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.hubs[chatID] != h {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) == 0 && h.gen == nil && !h.claimed {
		delete(e.hubs, chatID)
	}
}

// genActive reports whether the hub holds a running generation (a finalized
// generation kept only for the replay grace period does not count).
// Caller holds h.mu.
func genActive(h *chatHub) bool {
	if h.gen == nil {
		return false
	}
	h.gen.mu.Lock()
	defer h.gen.mu.Unlock()
	return !h.gen.done
}

// HasActiveGeneration reports whether the chat has a running generation.
func (e *Engine) HasActiveGeneration(chatID string) bool {
	h := e.hubIfExists(chatID)
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return genActive(h)
}

// ActiveGenerationChatIDs returns the IDs of all chats with a running
// generation. Powers the sidebar breathing-title reconciliation: background
// chats have no SSE stream attached, so the client polls this to learn when
// a background generation finished.
func (e *Engine) ActiveGenerationChatIDs() []string {
	type entry struct {
		id string
		h  *chatHub
	}
	e.mu.Lock()
	entries := make([]entry, 0, len(e.hubs))
	for id, h := range e.hubs {
		entries = append(entries, entry{id, h})
	}
	e.mu.Unlock()
	ids := make([]string, 0, len(entries))
	for _, en := range entries {
		en.h.mu.Lock()
		if genActive(en.h) {
			ids = append(ids, en.id)
		}
		en.h.mu.Unlock()
	}
	return ids
}

// globalEventTypes is the whitelist fanned out to the all-chats stream:
// sidebar-relevant lifecycle events plus config-change notifications. Per-chat
// deltas/tool events stay on the per-chat stream — the global one exists so
// clients can track background generations (breathing title) without
// attaching a stream to every chat.
var globalEventTypes = map[string]bool{
	"generation_started": true,
	"done":               true,
	"chat_updated":       true,
	"config_changed":     true,
}

// deliverGlobal fans an event out to global subscribers; a subscriber whose
// buffer is full is detached and must reconnect (the snapshot on subscribe
// covers the gap).
func (e *Engine) deliverGlobal(ev WireEvent) {
	if !globalEventTypes[ev.Type] {
		return
	}
	e.gmu.Lock()
	defer e.gmu.Unlock()
	for id, ch := range e.gsubs {
		select {
		case ch <- ev:
		default:
			close(ch)
			delete(e.gsubs, id)
		}
	}
}

// PublishConfigChanged tells global-stream subscribers that /api/config may
// now return different data (the MCP tool catalog is rebuilt asynchronously
// after a config save; this fires when that rebuild changed the catalog).
// Clients respond by refetching /api/config.
func (e *Engine) PublishConfigChanged() {
	e.deliverGlobal(WireEvent{Type: "config_changed"})
}

// SubscribeGlobal registers a subscriber for cross-chat lifecycle events.
// The first event is a generating_snapshot of chats with a running
// generation, so a (re)connecting client reconciles without polling.
func (e *Engine) SubscribeGlobal() (<-chan WireEvent, func()) {
	ch := make(chan WireEvent, 64)
	e.gmu.Lock()
	id := e.nextG
	e.nextG++
	e.gsubs[id] = ch
	e.gmu.Unlock()
	ch <- WireEvent{Type: "generating_snapshot", ChatIDs: e.ActiveGenerationChatIDs()}
	unsub := func() {
		e.gmu.Lock()
		delete(e.gsubs, id)
		e.gmu.Unlock()
	}
	return ch, unsub
}

// Subscribe returns the event channel for a chat and an unsubscribe func.
// Replay is lossless: the subscriber channel is sized to the replay buffer
// plus headroom, so every buffered event with seq > after is delivered.
// When no generation exists (and no grace-period buffer remains), an "idle"
// event is emitted immediately.
func (e *Engine) Subscribe(chatID string, after int64) (<-chan WireEvent, func()) {
	h := e.hubFor(chatID)
	h.mu.Lock()
	defer h.mu.Unlock()

	var replay []WireEvent
	if h.gen != nil {
		h.gen.mu.Lock()
		for _, ev := range h.gen.buffer {
			if ev.Seq > after {
				replay = append(replay, ev)
			}
		}
		h.gen.mu.Unlock()
	}

	// Sized so the full replay always fits, plus headroom for live events.
	ch := make(chan WireEvent, len(replay)+subscriberBuffer)
	id := h.nextSub
	h.nextSub++
	h.subs[id] = ch
	for _, ev := range replay {
		ch <- ev
	}
	if h.gen == nil {
		ch <- WireEvent{Type: "idle", Epoch: h.epoch}
	}
	unsub := func() {
		h.mu.Lock()
		delete(h.subs, id)
		h.mu.Unlock()
		e.maybePruneHub(chatID, h)
	}
	return ch, unsub
}

// BroadcastUserMessage publishes a user message created via REST (multi-client coherence).
func (e *Engine) BroadcastUserMessage(chatID string, msg *store.Message) {
	e.broadcastChat(chatID, WireEvent{Type: "user_message", Message: msg})
}

// BroadcastChatUpdated publishes a changed chat title (manual rename).
func (e *Engine) BroadcastChatUpdated(chatID, title string) {
	e.broadcastChat(chatID, WireEvent{Type: "chat_updated", Title: title})
}

// BroadcastSettingsUpdated publishes persisted per-chat settings with the
// updated chat payload so other clients can apply them without refetching.
func (e *Engine) BroadcastSettingsUpdated(chatID string, chat *store.Chat) {
	e.broadcastChat(chatID, WireEvent{Type: "settings_updated", Chat: chat})
}

// BroadcastMessagesReset tells clients the history was truncated
// (edit-and-resend or regenerate) and must be re-fetched.
func (e *Engine) BroadcastMessagesReset(chatID string) {
	e.broadcastChat(chatID, WireEvent{Type: "messages_reset"})
}

// broadcastChat delivers a chat-level event to current subscribers. It never
// creates a hub: a chat without a hub has no subscribers to notify, and
// creating hubs for every broadcast would grow engine.hubs unboundedly.
func (e *Engine) broadcastChat(chatID string, ev WireEvent) {
	h := e.hubIfExists(chatID)
	if h == nil {
		// No hub (no subscribers, no generation): per-chat delivery is a
		// no-op, but global subscribers must not be skipped — other tabs'
		// sidebars still need e.g. a manual rename of this idle chat.
		ev.ChatID = chatID
		e.deliverGlobal(ev)
		return
	}
	h.mu.Lock()
	h.publishChat(ev)
	h.mu.Unlock()
}

// ClaimGeneration atomically reserves the chat's generation slot BEFORE any
// user-visible persistence happens (TOCTOU fix): a handler claims, persists
// its user message / history edits, then calls StartClaimedGeneration — a
// concurrent request for the same chat gets ErrGenerationActive up front
// instead of a 409 after leaving an unanswered user message behind.
// On any failure between claim and start the handler MUST call
// ReleaseClaim.
func (e *Engine) ClaimGeneration(chatID string) error {
	h := e.hubFor(chatID)
	h.mu.Lock()
	defer h.mu.Unlock()
	if genActive(h) || h.claimed {
		return ErrGenerationActive
	}
	h.claimed = true
	return nil
}

// ReleaseClaim frees a slot reserved by ClaimGeneration.
func (e *Engine) ReleaseClaim(chatID string) {
	h := e.hubIfExists(chatID)
	if h == nil {
		return
	}
	h.mu.Lock()
	h.claimed = false
	h.mu.Unlock()
	e.maybePruneHub(chatID, h)
}

// startGeneration creates the assistant message and runs the turn loop
// asynchronously. Returns the assistant message. Conflict error if a
// generation is already active for the chat (a finalized generation kept
// only for the replay grace period is replaced, not a conflict).
// Test-only convenience: handlers claim, persist, then start.
func (e *Engine) startGeneration(ctx context.Context, chatID string) (*store.Message, error) {
	if err := e.ClaimGeneration(chatID); err != nil {
		return nil, err
	}
	return e.StartClaimedGeneration(ctx, chatID)
}

// StartClaimedGeneration creates the assistant message and runs the turn
// loop asynchronously on a slot previously reserved with ClaimGeneration
// (the handler path: claim → persist user-visible changes → start). The
// claim is always consumed: installed as the generation on success, released
// on failure. DB work runs OUTSIDE the hub lock so subscribers never block
// on DB latency.
func (e *Engine) StartClaimedGeneration(ctx context.Context, chatID string) (*store.Message, error) {
	h := e.hubFor(chatID)

	// Persistence here must survive the caller's (HTTP request) context
	// canceling: the user message is already persisted and the generation
	// itself runs on the server context (B6), so failing the assistant
	// message on a client disconnect would leave the user message
	// unanswered.
	persistCtx := context.WithoutCancel(ctx)

	// Stamp the assistant message with the model in effect NOW: the chat
	// model can be switched mid-conversation and history must stay accurate.
	model := e.cfg.Get().Models.DefaultChatModel
	if _, params, err := e.chatParams(persistCtx, chatID); err == nil && params.Model != "" {
		model = params.Model
	}
	am, err := e.store.CreateMessage(persistCtx, store.NewMessageParams{
		ChatID: chatID,
		Role:   store.RoleAssistant,
		Status: store.StatusGenerating,
		Model:  model,
	})
	if err != nil {
		e.ReleaseClaim(chatID)
		return nil, err
	}
	genCtx, cancel := context.WithCancel(e.serverCtx)
	ag := &activeGen{
		chatID:    chatID,
		messageID: am.ID,
		ctx:       genCtx,
		cancel:    cancel,
		attCache:  map[string]*store.Attachment{},
		attSent:   map[string]bool{},
	}

	h.mu.Lock()
	h.gen = ag
	h.claimed = false
	h.publishGen(WireEvent{Type: "generation_started"})
	h.mu.Unlock()

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.runGeneration(ag)
	}()
	return am, nil
}

// StopGeneration stops the active generation (keeps partial output). False if none.
func (e *Engine) StopGeneration(chatID string) bool {
	h := e.hubIfExists(chatID)
	if h == nil {
		return false
	}
	h.mu.Lock()
	active := genActive(h)
	ag := h.gen
	h.mu.Unlock()
	if !active {
		return false
	}
	ag.mu.Lock()
	ag.stopped = true
	ag.mu.Unlock()
	ag.cancel()
	return true
}

// CancelForChatDeletion aborts the generation without further persistence.
func (e *Engine) CancelForChatDeletion(chatID string) {
	h := e.hubIfExists(chatID)
	if h == nil {
		return
	}
	h.mu.Lock()
	ag := h.gen
	h.gen = nil
	h.mu.Unlock()
	if ag != nil {
		ag.mu.Lock()
		ag.deleted = true
		ag.mu.Unlock()
		ag.cancel()
	}
	e.dropHub(chatID)
}

// RecoverCrashed marks any in-flight messages from a previous server run as
// failed, inserting synthetic tool results for dangling calls first (B2).
func (e *Engine) RecoverCrashed(ctx context.Context) error {
	msgs, err := e.store.ListGeneratingMessages(ctx)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		if err := e.synthesizeToolResults(ctx, m.ChatID, m.ID, "Error: server restart interrupted generation"); err != nil {
			slog.Error("crash recovery: synthesize tool results", "message", m.ID, "error", err)
		}
		if err := e.store.FinalizeMessage(ctx, m.ID, store.StatusFailed,
			"interrupted (server restart)", m.Content, m.Reasoning); err != nil {
			slog.Error("crash recovery: finalize", "message", m.ID, "error", err)
		}
	}
	if len(msgs) > 0 {
		slog.Warn("crash recovery: marked interrupted generations", "count", len(msgs))
	}
	return nil
}

// shutdownWait bounds how long Shutdown waits for in-flight generations to
// finalize before giving up (finalization persists on detached contexts and
// is normally fast).
const shutdownWait = 5 * time.Second

// Shutdown cancels all active generations (invariant finalize marks them
// failed) and then WAITS for the runGeneration goroutines to finish their
// persistence, so a following DB close can't kill a write mid-flight.
func (e *Engine) Shutdown() {
	e.mu.Lock()
	hubs := make([]*chatHub, 0, len(e.hubs))
	for _, h := range e.hubs {
		hubs = append(hubs, h)
	}
	e.mu.Unlock()
	for _, h := range hubs {
		h.mu.Lock()
		ag := h.gen
		h.mu.Unlock()
		if ag != nil {
			ag.cancel()
		}
	}
	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(shutdownWait):
		slog.Warn("engine shutdown: timed out waiting for generations to finalize")
	}
}

// synthesizeToolResults inserts role=tool messages for every tool call of
// messageID lacking a result. This keeps history replayable to the API (B2).
func (e *Engine) synthesizeToolResults(ctx context.Context, chatID, messageID, text string) error {
	dangling, err := e.store.ListDanglingToolCalls(ctx, messageID)
	if err != nil {
		return err
	}
	for _, tc := range dangling {
		if _, err := e.store.CreateMessage(ctx, store.NewMessageParams{
			ChatID:     chatID,
			Role:       store.RoleTool,
			Status:     store.StatusComplete,
			Content:    text,
			ToolCallID: tc.ProviderCallID,
			Name:       tc.Name,
		}); err != nil {
			return err
		}
	}
	return nil
}
