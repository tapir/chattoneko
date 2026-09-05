package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// sseServer serves a fixed SSE body for POST /chat/completions.
func sseServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, body)
	}))
}

// captureServer serves a fixed SSE body and records the last request body.
func captureServer(t *testing.T, sseBody string) (*httptest.Server, func() []byte) {
	t.Helper()
	var mu sync.Mutex
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = b
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseBody)
	}))
	return srv, func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}

func collect(t *testing.T, es *EventStream) []StreamEvent {
	t.Helper()
	var out []StreamEvent
	for es.Next() {
		out = append(out, es.Event())
	}
	if err := es.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	return out
}

func TestChatCompletionsTextAndReasoning(t *testing.T) {
	// OpenRouter-style chunks: reasoning key (extra field) + content deltas.
	body := `data: {"id":"1","choices":[{"delta":{"role":"assistant","reasoning":"let me think"}}]}

data: {"id":"1","choices":[{"delta":{"content":"The answer"}}]}

data: {"id":"1","choices":[{"delta":{"content":" is 42"}}]}

data: {"id":"1","choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	srv := sseServer(t, body)
	defer srv.Close()

	p := newChatCompletionsProvider(srv.URL+"/", "test")
	es, err := p.StreamChat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, GenParams{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, es)

	var text, reasoning string
	var finish string
	for _, ev := range events {
		switch ev.Kind {
		case EventTextDelta:
			text += ev.Text
		case EventReasoningDelta:
			reasoning += ev.Text
		case EventDone:
			finish = ev.Finish
		}
	}
	if reasoning != "let me think" {
		t.Fatalf("reasoning = %q", reasoning)
	}
	if text != "The answer is 42" {
		t.Fatalf("text = %q", text)
	}
	if finish != "stop" {
		t.Fatalf("finish = %q", finish)
	}
}

func TestChatCompletionsUsage(t *testing.T) {
	// The terminal include_usage chunk carries no choices; its token counts
	// must surface on the EventDone event.
	body := `data: {"id":"1","choices":[{"delta":{"content":"hi"}}]}

data: {"id":"1","choices":[{"delta":{},"finish_reason":"stop"}]}

data: {"id":"1","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":7}}

data: [DONE]

`
	srv := sseServer(t, body)
	defer srv.Close()

	p := newChatCompletionsProvider(srv.URL+"/", "test")
	es, err := p.StreamChat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, GenParams{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, es)

	var got Usage
	var finish string
	for _, ev := range events {
		if ev.Kind == EventDone {
			got = ev.Usage
			finish = ev.Finish
		}
	}
	if got.PromptTokens != 12 || got.CompletionTokens != 7 {
		t.Fatalf("usage = %+v", got)
	}
	if finish != "stop" {
		t.Fatalf("finish = %q", finish)
	}
}

func TestChatCompletionsRequestBody(t *testing.T) {
	// Assert what actually goes on the wire: reasoning effort,
	// stream_options.include_usage, image data URLs and tool definitions.
	sse := `data: {"id":"1","choices":[{"delta":{"content":"ok"}}]}

data: {"id":"1","choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	srv, body := captureServer(t, sse)
	defer srv.Close()

	p := newChatCompletionsProvider(srv.URL+"/", "test")
	es, err := p.StreamChat(context.Background(),
		[]Message{{Role: "user", Content: "look", Images: []Image{{Data: []byte("png-bytes")}}}},
		[]Tool{{Name: "t1", Description: "d", Schema: json.RawMessage(`{"type":"object","properties":{}}`)}},
		GenParams{Model: "m", ReasoningEffort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, es)

	var req struct {
		Model           string           `json:"model"`
		ReasoningEffort string           `json:"reasoning_effort"`
		StreamOptions   map[string]any   `json:"stream_options"`
		Messages        []map[string]any `json:"messages"`
		Tools           []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(body(), &req); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if req.Model != "m" || req.ReasoningEffort != "high" {
		t.Fatalf("params = %+v", req)
	}
	if req.StreamOptions["include_usage"] != true {
		t.Fatalf("stream_options = %+v", req.StreamOptions)
	}
	if len(req.Tools) != 1 || req.Tools[0]["type"] != "function" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	msgs, _ := json.Marshal(req.Messages)
	if !strings.Contains(string(msgs), "data:image/png;base64,") {
		t.Fatalf("no png data url in messages: %s", msgs)
	}
}

func TestChatCompletionsToolCalls(t *testing.T) {
	// Tool-call deltas accumulated by index, mirroring OpenAI streaming.
	body := `data: {"id":"1","choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_abc","function":{"name":"get_weather","arguments":""}}]}}]}

data: {"id":"1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}

data: {"id":"1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	srv := sseServer(t, body)
	defer srv.Close()

	p := newChatCompletionsProvider(srv.URL+"/", "test")
	es, err := p.StreamChat(context.Background(), []Message{{Role: "user", Content: "weather?"}}, nil, GenParams{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, es)

	var started, deltas, done []StreamEvent
	var finish string
	for _, ev := range events {
		switch ev.Kind {
		case EventToolCallStart:
			started = append(started, ev)
		case EventToolCallDelta:
			deltas = append(deltas, ev)
		case EventToolCallDone:
			done = append(done, ev)
		case EventDone:
			finish = ev.Finish
		}
	}
	if len(started) != 1 || started[0].Name != "get_weather" {
		t.Fatalf("started events: %+v", started)
	}
	// Argument fragments stream as deltas AFTER the start event and
	// concatenate exactly to the done arguments.
	var streamed string
	for _, d := range deltas {
		if d.CallID != "call_abc" {
			t.Fatalf("delta call id = %q", d.CallID)
		}
		streamed += d.Args
	}
	if len(deltas) != 2 {
		t.Fatalf("deltas: %+v", deltas)
	}
	if len(done) != 1 || done[0].CallID != "call_abc" || done[0].Args != `{"city":"Paris"}` {
		t.Fatalf("done events: %+v", done)
	}
	if streamed != done[0].Args {
		t.Fatalf("streamed args %q != done args %q", streamed, done[0].Args)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish = %q", finish)
	}
}

func TestChatCompletionsToolCallStartWaitsForID(t *testing.T) {
	// Provider streams the tool-call NAME before the ID: no start event may
	// carry an empty call id; start fires once both are known. Argument
	// fragments arriving before the id are accumulated but NOT published
	// as deltas (the client couldn't attribute them).
	body := `data: {"id":"1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"get_weather"}}]}}]}

data: {"id":"1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"early\":"}}]}}]}

data: {"id":"1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_late","function":{"arguments":"1}"}}]},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	srv := sseServer(t, body)
	defer srv.Close()

	p := newChatCompletionsProvider(srv.URL+"/", "test")
	es, err := p.StreamChat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, GenParams{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, es)
	var started, done []StreamEvent
	for _, ev := range events {
		switch ev.Kind {
		case EventToolCallStart:
			started = append(started, ev)
		case EventToolCallDone:
			done = append(done, ev)
		}
	}
	if len(started) != 1 || started[0].CallID != "call_late" || started[0].Name != "get_weather" {
		t.Fatalf("started = %+v", started)
	}
	if len(done) != 1 || done[0].CallID != "call_late" || done[0].Args != `{"early":1}` {
		t.Fatalf("done = %+v", done)
	}
	// Only the fragment seen AFTER the start (id known) may be a delta; the
	// earlier {"early": fragment was silently accumulated and is re-delivered
	// by the done event.
	var deltas []StreamEvent
	for _, ev := range events {
		if ev.Kind == EventToolCallDelta {
			deltas = append(deltas, ev)
		}
	}
	if len(deltas) != 1 || deltas[0].Args != "1}" {
		t.Fatalf("deltas = %+v", deltas)
	}
}

func TestChatCompletionsToolCallWithoutIDDropped(t *testing.T) {
	// A malformed stream that never delivers a call id must NOT produce a
	// done event: an empty id would be persisted into history and poison
	// every later request replaying that history.
	body := `data: {"id":"1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"ghost","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	srv := sseServer(t, body)
	defer srv.Close()

	p := newChatCompletionsProvider(srv.URL+"/", "test")
	es, err := p.StreamChat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, GenParams{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, es)
	for _, ev := range events {
		if ev.Kind == EventToolCallStart || ev.Kind == EventToolCallDone || ev.Kind == EventToolCallDelta {
			t.Fatalf("unexpected tool event: %+v", ev)
		}
	}
}

func TestChatCompletionsErrorMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"maximum context length exceeded"}}`)
	}))
	defer srv.Close()

	p := newChatCompletionsProvider(srv.URL+"/", "test")
	es, err := p.StreamChat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, GenParams{Model: "m"})
	if err != nil {
		t.Fatalf("unexpected sync error: %v", err)
	}
	var sawErr error
	for es.Next() {
		if ev := es.Event(); ev.Kind == EventError {
			sawErr = ev.Err
		}
	}
	if sawErr == nil && es.Err() == nil {
		t.Fatal("expected an error event")
	}
	if got := fmt.Sprint(sawErr, es.Err()); !strings.Contains(got, "context length") {
		t.Fatalf("error text %q does not carry the provider message", got)
	}
}

func TestChatCompletionsRequestValidation(t *testing.T) {
	p := newChatCompletionsProvider("http://unused/", "test")

	if _, err := p.StreamChat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, GenParams{}); err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("empty model: err = %v", err)
	}
	if _, err := p.StreamChat(context.Background(), []Message{{Role: "bogus"}}, nil, GenParams{Model: "m"}); err == nil || !strings.Contains(err.Error(), "unknown message role") {
		t.Fatalf("unknown role: err = %v", err)
	}
	if _, err := p.StreamChat(context.Background(), []Message{{Role: "user", Content: "hi"}},
		[]Tool{{Name: "bad", Schema: json.RawMessage("{not json")}}, GenParams{Model: "m"}); err == nil || !strings.Contains(err.Error(), "invalid JSON schema") {
		t.Fatalf("invalid schema: err = %v", err)
	}
}

// TestChatCompletionsIdleTimeout locks in the silent-provider watchdog: a
// server that sends one chunk and then never closes the stream must abort
// the generation with a clear error instead of holding it open forever.
func TestChatCompletionsIdleTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"1","choices":[{"delta":{"content":"hi"}}]}

`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // hang until the client's watchdog cancels
	}))
	defer srv.Close()

	p := newChatCompletionsProvider(srv.URL+"/", "test")
	p.idleTimeout = 100 * time.Millisecond
	es, err := p.StreamChat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, GenParams{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var sawText bool
	var sawErr error
	for es.Next() {
		ev := es.Event()
		if ev.Kind == EventTextDelta {
			sawText = true
		}
		if ev.Kind == EventError {
			sawErr = ev.Err
		}
	}
	if !sawText {
		t.Fatal("first chunk lost")
	}
	if sawErr == nil || !strings.Contains(sawErr.Error(), "without any data") {
		t.Fatalf("want idle-timeout error, got %v", sawErr)
	}
	if es.Err() == nil {
		t.Fatal("stream Err() must carry the idle error")
	}
}

// EventStream contract tests.

func TestEventStreamCloseUnblocksProducer(t *testing.T) {
	// A producer blocked on a full buffer must be unblocked by the
	// consumer's Close (otherwise a stopped generation leaks the producer
	// goroutine and its HTTP connection).
	es := NewEventStream(1, nil)
	prodDone := make(chan struct{})
	go func() {
		defer close(prodDone)
		for i := 0; i < 1000; i++ {
			if !es.Publish(StreamEvent{Kind: EventTextDelta, Text: "x"}) {
				return // consumer went away
			}
		}
		es.Finish(nil)
	}()
	time.Sleep(20 * time.Millisecond) // let the producer fill the buffer and block
	es.Close()
	select {
	case <-prodDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock the producer")
	}
}

func TestEventStreamFinishIsIdempotent(t *testing.T) {
	es := NewEventStream(4, nil)
	if !es.Publish(StreamEvent{Kind: EventTextDelta, Text: "a"}) {
		t.Fatal("publish failed")
	}
	es.Finish(errors.New("boom"))
	es.Finish(errors.New("second finish must not panic"))
	if es.Publish(StreamEvent{Kind: EventTextDelta, Text: "b"}) {
		t.Fatal("publish after Finish must return false")
	}
	if !es.Next() || es.Event().Text != "a" {
		t.Fatal("expected exactly one event")
	}
	if es.Next() {
		t.Fatal("stream should be exhausted")
	}
	if es.Err() == nil || es.Err().Error() != "boom" {
		t.Fatalf("Err() = %v", es.Err())
	}
}

func TestEventStreamCloseCancelsOnce(t *testing.T) {
	n := 0
	es := NewEventStream(4, func() { n++ })
	es.Close()
	es.Close()
	if n != 1 {
		t.Fatalf("cancel called %d times, want 1", n)
	}
}
