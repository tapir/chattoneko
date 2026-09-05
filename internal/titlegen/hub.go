package titlegen

import "sync"

// subscriberBuffer is deliberately generous: title events are one-per-chat,
// so a full buffer means the subscriber is truly stuck.
const subscriberBuffer = 64

// TitleEvent is the wire payload of the title stream: a chat's title became
// final. Self-contained and idempotent (chat id + the title itself), so a
// missed event only delays a sidebar label until the next chat-list load —
// no replay buffer is needed.
type TitleEvent struct {
	ChatID string `json:"chat_id"`
	Title  string `json:"title"`
}

// Hub is the title stream's fan-out registry. It is fully independent from
// the engine's hub/global-stream machinery: its own mutex, its own
// subscriber channels, and exactly one writer (the title task). Title
// delivery can never be queued behind, or dropped because of, chat
// generation traffic.
type Hub struct {
	mu   sync.Mutex
	subs map[int]chan TitleEvent
	next int
}

// NewHub creates an empty Hub.
func NewHub() *Hub {
	return &Hub{subs: map[int]chan TitleEvent{}}
}

// Publish delivers a title event to every subscriber. Non-blocking: a
// subscriber whose buffer is full is detached (its channel is closed) and
// reconnects via the EventSource's built-in retry — one slow client never
// stalls the others.
func (h *Hub) Publish(chatID, title string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, ch := range h.subs {
		select {
		case ch <- TitleEvent{ChatID: chatID, Title: title}:
		default:
			close(ch)
			delete(h.subs, id)
		}
	}
}

// Subscribe registers a subscriber and returns its event channel plus an
// unsubscribe func.
func (h *Hub) Subscribe() (<-chan TitleEvent, func()) {
	ch := make(chan TitleEvent, subscriberBuffer)
	h.mu.Lock()
	id := h.next
	h.next++
	h.subs[id] = ch
	h.mu.Unlock()
	unsub := func() {
		h.mu.Lock()
		delete(h.subs, id)
		h.mu.Unlock()
	}
	return ch, unsub
}
