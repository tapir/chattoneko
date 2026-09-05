// Package provider talks to the single OpenAI-compatible LLM endpoint via
// POST /chat/completions. types.go defines the normalized wire contract;
// chat_completions.go implements it.
package provider

import (
	"context"
	"encoding/json"
	"sync"
)

// GenParams carries the generation options for one request.
type GenParams struct {
	Model           string
	ReasoningEffort string // "" | low | medium | high — sent only when non-empty
}

// Tool is a tool definition sent to the provider.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage // JSON Schema for the arguments object
}

// Image is an image attached to a user message. Data MUST be PNG-encoded:
// it is sent verbatim as a data:image/png URL (the engine re-encodes all
// uploaded images to PNG before handing them to the provider).
type Image struct {
	Data []byte // PNG-encoded bytes
}

// ToolCall is a tool invocation requested by the assistant.
type ToolCall struct {
	ID        string // provider-issued call id (e.g. call_xxx)
	Name      string
	Arguments string // JSON object as a string
}

// Message is the normalized conversation message exchanged with the engine.
type Message struct {
	Role    string // system | user | assistant | tool
	Content string // text content (tool result text when Role=tool)

	Images []Image    // user messages only
	Calls  []ToolCall // assistant messages only

	ToolCallID string // tool messages only: the call this result answers
}

// EventKind enumerates normalized stream events.
type EventKind int

const (
	EventTextDelta      EventKind = iota // Text carries the delta
	EventReasoningDelta                  // Text carries the reasoning delta
	EventToolCallStart                   // CallID + Name (name may be "" until known)
	EventToolCallDelta                   // CallID + Args: an incremental arguments JSON fragment (start already emitted)
	EventToolCallDone                    // CallID + Name + Args complete
	EventDone                            // stream finished; Finish carries finish reason
	EventError                           // Err carries the error
)

// Usage captures token accounting for a single generation request, when the
// provider reports it. Zero fields = not reported.
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
}

// StreamEvent is one normalized event from the provider stream.
type StreamEvent struct {
	Kind   EventKind
	Text   string
	CallID string
	Name   string
	Args   string
	Finish string // set on EventDone: "stop" | "tool_calls" | "length" | ...
	Err    error  // set on EventError
	Usage  Usage  // set on EventDone: token usage (0 = not reported)
}

// EventStream is a pull-based iterator over stream events.
//
// Concurrency contract: ONE producer goroutine calls Publish and Finish;
// the consumer calls Next/Event/Err and may call Close at any time from any
// goroutine. The channel is buffered; Next blocks until an event arrives or
// the stream closes. After Next returns false, Err reports the terminal
// error (nil on clean finish).
//
// Close is the consumer-side cancel: it unblocks a producer sitting in
// Publish (which then returns false) and runs the cancel function the
// producer registered in NewEventStream.
type EventStream struct {
	ch         chan StreamEvent
	cur        StreamEvent
	errMu      sync.Mutex // guards err (consumer may call Err while the producer is mid-Finish)
	err        error
	done       bool // producer-only: Finish has been called
	stopOnce   sync.Once
	stopped    chan struct{} // closed by consumer Close; unblocks producers
	finishOnce sync.Once
	cancel     func() // cancels the producing goroutine's request context (may be nil)
}

// NewEventStream creates a stream with the given buffer size (values <= 0
// select a default). cancel is invoked by the consumer's Close to abort the
// producer's underlying request (may be nil). The producer must Publish its
// events and then call Finish exactly once.
func NewEventStream(buffer int, cancel func()) *EventStream {
	if buffer <= 0 {
		buffer = 256
	}
	return &EventStream{
		ch:      make(chan StreamEvent, buffer),
		stopped: make(chan struct{}),
		cancel:  cancel,
	}
}

// Publish sends an event to the consumer. It returns false when the
// consumer went away (Close) — the producer should stop. The send blocks on
// a full buffer until the consumer reads or closes; a consumer that called
// Close always unblocks a pending publish (otherwise a stopped generation
// with a full buffer would leak the producer goroutine).
func (s *EventStream) Publish(ev StreamEvent) bool {
	if s.done {
		return false
	}
	select {
	case s.ch <- ev:
		return true
	case <-s.stopped:
		return false
	}
}

// Finish closes the stream. err is nil on clean completion (EventDone should
// have been published already); non-nil err is stored for Err(). Finish is
// idempotent: only the first call takes effect.
func (s *EventStream) Finish(err error) {
	s.finishOnce.Do(func() {
		s.done = true
		s.errMu.Lock()
		s.err = err
		s.errMu.Unlock()
		close(s.ch)
	})
}

// Next advances to the next event.
func (s *EventStream) Next() bool {
	ev, ok := <-s.ch
	if !ok {
		return false
	}
	s.cur = ev
	return true
}

// Event returns the current event.
func (s *EventStream) Event() StreamEvent { return s.cur }

// Err returns the terminal error. Safe to call at any time; meaningful once
// Next returns false.
func (s *EventStream) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

// Close aborts the stream (consumer-side cancel) and unblocks any producer
// waiting in Publish. Idempotent; the cancel function runs exactly once.
func (s *EventStream) Close() {
	s.stopOnce.Do(func() {
		close(s.stopped)
		if s.cancel != nil {
			s.cancel()
		}
	})
}

// Provider abstracts the single OpenAI-compatible endpoint.
type Provider interface {
	// StreamChat runs a streaming completion over the normalized messages.
	StreamChat(ctx context.Context, msgs []Message, tools []Tool, p GenParams) (*EventStream, error)
}
