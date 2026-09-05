// Package engine runs LLM generations: the turn loop, the active-generation
// registry, and the per-chat SSE fan-out hub.
package engine

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"

	"chattoneko/internal/store"
)

// WireEvent is one SSE event (the doc's wire format).
type WireEvent struct {
	Seq int64 `json:"seq,omitempty"`
	// Epoch identifies the hub incarnation that stamped the seq space.
	// Seq is only monotonic within one hub lifetime: pruning a hub and
	// recreating it starts seq over at 1. Clients seeing a new epoch must
	// reset their dedupe baseline, or a stale lastSeq silently drops every
	// event of the next generation.
	Epoch     string `json:"epoch,omitempty"`
	Type      string `json:"type"`
	ChatID    string `json:"chat_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	Content   string `json:"content,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Result    string `json:"result,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	Status    string `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
	Title     string `json:"title,omitempty"`
	// Per-turn usage + duration, set on the "done" event (#5 + top-bar totals).
	PromptTokens     int64          `json:"prompt_tokens,omitempty"`
	CompletionTokens int64          `json:"completion_tokens,omitempty"`
	DurationMs       int64          `json:"duration_ms,omitempty"`
	Message          *store.Message `json:"message,omitempty"`
	Chat             *store.Chat    `json:"chat,omitempty"`
	// Attachment is set on attachment_created: a tool-generated file was
	// linked to the generating assistant message (download chip in the UI).
	Attachment *store.AttachmentMeta `json:"attachment,omitempty"`
	// ChatIDs is set on the global stream's generating_snapshot event.
	ChatIDs []string `json:"chat_ids,omitempty"`
}

const subscriberBuffer = 1024

// chatHub is the per-chat fan-out hub. One generation at a time.
type chatHub struct {
	mu      sync.Mutex
	id      string
	epoch   string // hub incarnation id, stamped on every event (see WireEvent)
	gen     *activeGen
	claimed bool // generation slot reserved (between ClaimGeneration and StartClaimedGeneration)
	subs    map[int]chan WireEvent
	nextSub int
	seq     int64 // chat-scoped monotonic event sequence; persists across generations
	// global fans sidebar-relevant lifecycle events out to the engine's
	// all-chats stream subscribers (see Engine.deliverGlobal).
	global func(WireEvent)
}

// hubEpochs numbers hub incarnations so clients can detect a seq-space
// reset across hub pruning/recreation.
var hubEpochs atomic.Int64

func newChatHub(id string, global func(WireEvent)) *chatHub {
	return &chatHub{id: id, epoch: strconv.FormatInt(hubEpochs.Add(1), 36), subs: map[int]chan WireEvent{}, global: global}
}

// deliver sends an event to every subscriber; a subscriber whose buffer is
// full is detached (its channel is closed) and must reconnect — the replay
// buffer covers the gap. The event is also stamped with the chat id and
// fanned out to global (all-chats) subscribers, which filter by type.
func (h *chatHub) deliver(ev WireEvent) {
	ev.ChatID = h.id
	for id, ch := range h.subs {
		select {
		case ch <- ev:
		default:
			close(ch)
			delete(h.subs, id)
		}
	}
	if h.global != nil {
		h.global(ev)
	}
}

// activeGen tracks one in-flight generation.
type activeGen struct {
	mu        sync.Mutex
	chatID    string
	messageID string
	ctx       context.Context    // generation ctx (child of server ctx)
	cancel    context.CancelFunc // stop/deletion/shutdown only
	buffer    []WireEvent        // generation events only (replayable)
	text      string
	reasoning string
	dirty     bool
	stopped   bool // user requested stop
	deleted   bool // chat was deleted; skip persistence on finalize
	done      bool // finalized; kept briefly for late-subscriber replay (grace period)
	// attCache memoizes attachment blobs fetched while building provider
	// messages (they don't change mid-generation). Only the runGeneration
	// goroutine touches it — no lock needed.
	attCache map[string]*store.Attachment
	// attSent tracks attachment ids already published as attachment_created
	// (tool-generated files), so scanning after each tool call publishes each
	// file exactly once. Only the runGeneration goroutine touches it.
	attSent map[string]bool
}

// publishGen appends a generation event to the replay buffer with the next
// chat-scoped seq and delivers it. Caller holds chatHub.mu.
func (h *chatHub) publishGen(ev WireEvent) {
	if h.gen == nil {
		return
	}
	h.seq++
	ev.Seq = h.seq
	ev.Epoch = h.epoch
	ev.MessageID = h.gen.messageID
	h.gen.mu.Lock()
	h.gen.buffer = append(h.gen.buffer, ev)
	h.gen.mu.Unlock()
	h.deliver(ev)
}

// publishChat broadcasts a chat-level event (not replayed, no seq).
// Exception: chat_updated is appended to the grace-period replay buffer with
// a seq. A rename can land right after done, and a subscriber reconnecting
// in that window must not lose it — unreplayed, a dropped chat_updated left
// the sidebar title stale until a full reload.
// Caller holds chatHub.mu.
func (h *chatHub) publishChat(ev WireEvent) {
	ev.Epoch = h.epoch
	if ev.Type == "chat_updated" && h.gen != nil {
		h.seq++
		ev.Seq = h.seq
		h.gen.mu.Lock()
		h.gen.buffer = append(h.gen.buffer, ev)
		h.gen.mu.Unlock()
	}
	h.deliver(ev)
}
