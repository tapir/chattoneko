package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"chattoneko/internal/config"
	"chattoneko/internal/db"
	"chattoneko/internal/mcphub"
	"chattoneko/internal/provider"
	"chattoneko/internal/store"
	"chattoneko/internal/tools"
)

// ---- fakes ----

type scriptedProvider struct {
	scripts   [][]provider.StreamEvent // one script per StreamChat call
	callIndex int
}

func (f *scriptedProvider) StreamChat(_ context.Context, _ []provider.Message, _ []provider.Tool, _ provider.GenParams) (*provider.EventStream, error) {
	script := f.scripts[f.callIndex]
	f.callIndex++
	es := provider.NewEventStream(64, nil)
	go func() {
		for _, ev := range script {
			if !es.Publish(ev) {
				return
			}
		}
		es.Finish(nil)
	}()
	return es, nil
}

type fakeMCP struct {
	tools   []mcphub.Entry
	results map[string]string
	calls   []string
}

func (f *fakeMCP) Tools() []mcphub.Entry { return f.tools }

func (f *fakeMCP) Call(_ context.Context, name, _ string, _ mcphub.CallMeta) (string, bool, error) {
	f.calls = append(f.calls, name)
	return f.results[name], false, nil
}

// ---- harness ----

func testEngine(t *testing.T, prov provider.Provider, m ToolCatalog) (*Engine, *store.Store, context.CancelFunc) {
	t.Helper()
	// A temp FILE per test keeps each test's database fully isolated.
	sqlDB, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := store.NewStore(sqlDB)
	cfgs, err := config.TestStore(context.Background(), sqlDB, config.Config{
		Models: config.ModelsConfig{DefaultChatModel: "m"},
		Limits: config.LimitsConfig{MaxToolIterations: 10},
	})
	if err != nil {
		t.Fatalf("config store: %v", err)
	}
	serverCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = sqlDB.Close() })
	return New(serverCtx, st, prov, m, cfgs, nil), st, cancel
}

func newTestChat(t *testing.T, st *store.Store) string {
	t.Helper()
	chat, err := st.CreateChat(context.Background(), "m", store.GenParams{}, map[string]bool{})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	return chat.ID
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// ---- tests ----

func TestBasicGenerationPersistsAndStreams(t *testing.T) {
	prov := &scriptedProvider{scripts: [][]provider.StreamEvent{{
		{Kind: provider.EventTextDelta, Text: "Hello "},
		{Kind: provider.EventTextDelta, Text: "world"},
		{Kind: provider.EventDone, Finish: "stop"},
	}}}
	eng, st, _ := testEngine(t, prov, &fakeMCP{})
	chatID := newTestChat(t, st)
	if _, err := st.CreateMessage(context.Background(), store.NewMessageParams{
		ChatID: chatID, Role: store.RoleUser, Status: store.StatusComplete, Content: "hi",
	}); err != nil {
		t.Fatal(err)
	}

	ch, unsub := eng.Subscribe(chatID, 0)
	defer unsub()

	am, err := eng.startGeneration(context.Background(), chatID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	waitFor(t, "generation complete", 5*time.Second, func() bool {
		m, err := st.GetMessage(context.Background(), am.ID)
		return err == nil && m.Status == store.StatusComplete
	})

	m, _ := st.GetMessage(context.Background(), am.ID)
	if m.Content != "Hello world" {
		t.Fatalf("content = %q", m.Content)
	}

	// Drain remaining events; we must see status+done.
	sawDone := false
	timeout := time.After(2 * time.Second)
	for !sawDone {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("channel closed before done")
			}
			if ev.Type == "done" {
				sawDone = true
			}
		case <-timeout:
			t.Fatal("no done event")
		}
	}
}

func TestTruncationFinishMarksFailed(t *testing.T) {
	for _, reason := range []string{"length", "max_tokens", "max_output_tokens"} {
		t.Run(reason, func(t *testing.T) {
			prov := &scriptedProvider{scripts: [][]provider.StreamEvent{{
				{Kind: provider.EventTextDelta, Text: "partial answer"},
				{Kind: provider.EventDone, Finish: reason},
			}}}
			eng, st, _ := testEngine(t, prov, &fakeMCP{})
			chatID := newTestChat(t, st)
			if _, err := st.CreateMessage(context.Background(), store.NewMessageParams{
				ChatID: chatID, Role: store.RoleUser, Status: store.StatusComplete, Content: "hi",
			}); err != nil {
				t.Fatal(err)
			}
			am, err := eng.startGeneration(context.Background(), chatID)
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			waitFor(t, "generation terminal", 5*time.Second, func() bool {
				m, err := st.GetMessage(context.Background(), am.ID)
				return err == nil && m.Status == store.StatusFailed
			})
			m, _ := st.GetMessage(context.Background(), am.ID)
			if m.Content != "partial answer" {
				t.Fatalf("partial content lost: %q", m.Content)
			}
			if m.Error == "" {
				t.Fatal("truncated message should carry an error explaining the cut-off")
			}
		})
	}
}

func TestToolLoopExecutesAndReplaysHistory(t *testing.T) {
	prov := &scriptedProvider{
		scripts: [][]provider.StreamEvent{
			{ // iteration 1: model calls a tool
				{Kind: provider.EventToolCallDone, CallID: "call_1", Name: "echo", Args: `{"text":"x"}`},
				{Kind: provider.EventDone, Finish: "tool_calls"},
			},
			{ // iteration 2: model answers
				{Kind: provider.EventTextDelta, Text: "done"},
				{Kind: provider.EventDone, Finish: "stop"},
			},
		},
	}
	mcpFake := &fakeMCP{
		tools:   []mcphub.Entry{{Display: "echo", Description: "d", Server: "s", DefaultEnabled: true}},
		results: map[string]string{"echo": "echo-result"},
	}
	eng, st, _ := testEngine(t, prov, mcpFake)
	chatID := newTestChat(t, st)
	if _, err := st.CreateMessage(context.Background(), store.NewMessageParams{
		ChatID: chatID, Role: store.RoleUser, Status: store.StatusComplete, Content: "call it",
	}); err != nil {
		t.Fatal(err)
	}

	am, err := eng.startGeneration(context.Background(), chatID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "tool-loop complete", 5*time.Second, func() bool {
		m, err := st.GetMessage(context.Background(), am.ID)
		return err == nil && m.Status == store.StatusComplete
	})

	msgs, _ := st.ListMessages(context.Background(), chatID)
	// user, assistant(tool_calls), tool result
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages, got %d", len(msgs))
	}
	if len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].ProviderCallID != "call_1" {
		t.Fatalf("assistant tool calls wrong: %+v", msgs[1].ToolCalls)
	}
	if msgs[2].Role != store.RoleTool || msgs[2].ToolCallID != "call_1" || msgs[2].Content != "echo-result" {
		t.Fatalf("tool result wrong: %+v", msgs[2])
	}
	if len(mcpFake.calls) != 1 || mcpFake.calls[0] != "echo" {
		t.Fatalf("mcp calls: %v", mcpFake.calls)
	}
}

// blockingTool is a catalog whose only tool signals when it starts and does
// not complete until released — letting a test stop the generation exactly
// while the tool is "running".
type blockingTool struct {
	entry    mcphub.Entry
	started  chan string
	released chan struct{}
}

func (b *blockingTool) Tools() []mcphub.Entry { return []mcphub.Entry{b.entry} }

func (b *blockingTool) Call(_ context.Context, name, _ string, _ mcphub.CallMeta) (string, bool, error) {
	b.started <- name
	<-b.released
	return "TOOL_RAN", false, nil
}

// A tool that finished executing must keep its result even when a user stop
// lands before the result is persisted: the write happens on a detached
// context, so the finalize synthesis never fakes "interrupted before this
// tool call could run" and the model can never re-run the tool's side
// effects on the next generation.
func TestStopAfterToolStillPersistsResult(t *testing.T) {
	prov := &scriptedProvider{
		scripts: [][]provider.StreamEvent{
			{ // turn 1: model calls the blocking tool
				{Kind: provider.EventToolCallDone, CallID: "call_1", Name: "slow_tool", Args: "{}"},
				{Kind: provider.EventDone, Finish: "tool_calls"},
			},
			{ // turn 2: never reached (the stop wins at the loop top)
				{Kind: provider.EventDone, Finish: "stop"},
			},
		},
	}
	tool := &blockingTool{
		entry:    mcphub.Entry{Display: "slow_tool", Description: "d", Server: "s", DefaultEnabled: true},
		started:  make(chan string, 1),
		released: make(chan struct{}),
	}
	eng, st, _ := testEngine(t, prov, tool)
	chatID := newTestChat(t, st)
	if _, err := st.CreateMessage(context.Background(), store.NewMessageParams{
		ChatID: chatID, Role: store.RoleUser, Status: store.StatusComplete, Content: "run it",
	}); err != nil {
		t.Fatal(err)
	}

	am, err := eng.startGeneration(context.Background(), chatID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// Stop exactly while the tool is "running": its result is not back yet,
	// so persisting it races the cancel.
	<-tool.started
	if !eng.StopGeneration(chatID) {
		t.Fatal("stop: no active generation")
	}
	close(tool.released)

	waitFor(t, "generation stopped", 5*time.Second, func() bool {
		m, err := st.GetMessage(context.Background(), am.ID)
		return err == nil && m.Status == store.StatusStopped
	})
	// The executed tool's real result is persisted — not a synthesized
	// "interrupted" placeholder.
	msgs, err := st.ListMessages(context.Background(), chatID)
	if err != nil {
		t.Fatal(err)
	}
	var toolMsg *store.Message
	for _, m := range msgs {
		if m.Role == store.RoleTool && m.ToolCallID == "call_1" {
			toolMsg = m
		}
	}
	if toolMsg == nil {
		t.Fatal("tool result message missing")
	}
	if toolMsg.Content != "TOOL_RAN" {
		t.Fatalf("tool result = %q, want the real TOOL_RAN (not a synthesized interrupt)", toolMsg.Content)
	}
}

// Tool-call argument fragments stream to SSE subscribers between the start
// and done events (UI renders the arguments filling in live).
func TestToolCallDeltasStreamToSubscribers(t *testing.T) {
	prov := &scriptedProvider{
		scripts: [][]provider.StreamEvent{
			{ // iteration 1: tool call with streamed arguments
				{Kind: provider.EventToolCallStart, CallID: "call_1", Name: "echo"},
				{Kind: provider.EventToolCallDelta, CallID: "call_1", Args: `{"text"`},
				{Kind: provider.EventToolCallDelta, CallID: "call_1", Args: `:"x"}`},
				{Kind: provider.EventToolCallDone, CallID: "call_1", Name: "echo", Args: `{"text":"x"}`},
				{Kind: provider.EventDone, Finish: "tool_calls"},
			},
			{ // iteration 2: model answers
				{Kind: provider.EventTextDelta, Text: "done"},
				{Kind: provider.EventDone, Finish: "stop"},
			},
		},
	}
	mcpFake := &fakeMCP{
		tools:   []mcphub.Entry{{Display: "echo", Description: "d", Server: "s", DefaultEnabled: true}},
		results: map[string]string{"echo": "echo-result"},
	}
	eng, st, _ := testEngine(t, prov, mcpFake)
	chatID := newTestChat(t, st)
	if _, err := st.CreateMessage(context.Background(), store.NewMessageParams{
		ChatID: chatID, Role: store.RoleUser, Status: store.StatusComplete, Content: "call it",
	}); err != nil {
		t.Fatal(err)
	}

	ch, unsub := eng.Subscribe(chatID, 0)
	defer unsub()

	if _, err := eng.startGeneration(context.Background(), chatID); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Collect tool-call wire events in order until done.
	var got []WireEvent
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("channel closed before done")
			}
			switch ev.Type {
			case "tool_call_started", "tool_call_delta", "tool_call_done":
				got = append(got, ev)
			case "done":
				goto check
			}
		case <-timeout:
			t.Fatal("no done event")
		}
	}
check:
	if len(got) != 4 {
		t.Fatalf("tool-call events = %+v", got)
	}
	if got[0].Type != "tool_call_started" || got[0].CallID != "call_1" || got[0].Name != "echo" {
		t.Fatalf("started = %+v", got[0])
	}
	if got[1].Type != "tool_call_delta" || got[1].CallID != "call_1" || got[1].Arguments != `{"text"` {
		t.Fatalf("delta 1 = %+v", got[1])
	}
	if got[2].Type != "tool_call_delta" || got[2].Arguments != `:"x"}` {
		t.Fatalf("delta 2 = %+v", got[2])
	}
	if got[3].Type != "tool_call_done" || got[3].Arguments != `{"text":"x"}` {
		t.Fatalf("done = %+v", got[3])
	}
}

func TestStopKeepsPartialAndMarksStopped(t *testing.T) {
	// The provider streams a prefix then blocks forever (until ctx cancel).
	prov := &blockingProvider{prefix: "partial text"}
	eng, st, _ := testEngine(t, prov, &fakeMCP{})
	chatID := newTestChat(t, st)
	if _, err := st.CreateMessage(context.Background(), store.NewMessageParams{
		ChatID: chatID, Role: store.RoleUser, Status: store.StatusComplete, Content: "go",
	}); err != nil {
		t.Fatal(err)
	}
	am, err := eng.startGeneration(context.Background(), chatID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "partial content", 5*time.Second, func() bool {
		m, _ := st.GetMessage(context.Background(), am.ID)
		return m != nil && m.Content == "partial text"
	})
	if !eng.StopGeneration(chatID) {
		t.Fatal("stop reported no active generation")
	}
	waitFor(t, "stopped status", 5*time.Second, func() bool {
		m, err := st.GetMessage(context.Background(), am.ID)
		return err == nil && m.Status == store.StatusStopped
	})
	m, _ := st.GetMessage(context.Background(), am.ID)
	if m.Content != "partial text" {
		t.Fatalf("partial content lost: %q", m.Content)
	}
}

// blockingProvider streams a prefix then waits for ctx cancellation.
type blockingProvider struct{ prefix string }

func (b *blockingProvider) StreamChat(ctx context.Context, _ []provider.Message, _ []provider.Tool, _ provider.GenParams) (*provider.EventStream, error) {
	es := provider.NewEventStream(64, nil)
	go func() {
		es.Publish(provider.StreamEvent{Kind: provider.EventTextDelta, Text: b.prefix})
		<-ctx.Done()
		es.Finish(ctx.Err())
	}()
	return es, nil
}

func TestInvariantFinalizeSynthesizesToolResults(t *testing.T) {
	// First iteration ends in tool_calls; stop arrives during tool execution
	// phase simulation: we emulate the crash path instead — an assistant
	// message left with dangling tool calls must get synthetic results.
	eng, st, _ := testEngine(t, &scriptedProvider{}, &fakeMCP{})
	chatID := newTestChat(t, st)
	am, err := st.CreateMessage(context.Background(), store.NewMessageParams{
		ChatID: chatID, Role: store.RoleAssistant, Status: store.StatusGenerating,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateToolCall(context.Background(), am.ID, "call_dangling", "echo", `{"a":1}`, 0); err != nil {
		t.Fatal(err)
	}

	// Crash recovery path: generating → failed + synthetic tool result.
	if err := eng.RecoverCrashed(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	msgs, _ := st.ListMessages(context.Background(), chatID)
	if len(msgs) != 2 {
		t.Fatalf("want assistant+synthetic tool, got %d", len(msgs))
	}
	m, _ := st.GetMessage(context.Background(), am.ID)
	if m.Status != store.StatusFailed || m.Error == "" {
		t.Fatalf("assistant not failed: %+v", m)
	}
	tool := msgs[1]
	if tool.Role != store.RoleTool || tool.ToolCallID != "call_dangling" {
		t.Fatalf("synthetic tool result wrong: %+v", tool)
	}
}

func TestSubscribeReplayBuffer(t *testing.T) {
	prov := &scriptedProvider{scripts: [][]provider.StreamEvent{{
		{Kind: provider.EventTextDelta, Text: "abc"},
		{Kind: provider.EventTextDelta, Text: "def"},
		{Kind: provider.EventDone, Finish: "stop"},
	}}}
	eng, st, _ := testEngine(t, prov, &fakeMCP{})
	chatID := newTestChat(t, st)
	if _, err := st.CreateMessage(context.Background(), store.NewMessageParams{
		ChatID: chatID, Role: store.RoleUser, Status: store.StatusComplete, Content: "hi",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.startGeneration(context.Background(), chatID); err != nil {
		t.Fatal(err)
	}
	// Late subscriber joining after completion (within the ~5s grace period) must
	// receive the FULL replay of the finished generation, not idle. Idle is
	// emitted only once the buffer has been dropped after the grace period.
	waitFor(t, "completion", 5*time.Second, func() bool {
		return !eng.HasActiveGeneration(chatID)
	})
	ch, unsub := eng.Subscribe(chatID, 0)
	defer unsub()
	var deltas int
	sawDone := false
	timeout := time.After(2 * time.Second)
	for !sawDone {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("channel closed before replay completed")
			}
			if ev.Type == "delta" {
				deltas++
			}
			if ev.Type == "done" {
				sawDone = true
			}
		case <-timeout:
			t.Fatal("no done event in replay")
		}
	}
	if deltas != 2 {
		t.Fatalf("replay delivered %d deltas, want 2", deltas)
	}
}

// TestClaimGenerationIsAtomic verifies the TOCTOU fix: only one claimant can
// hold the generation slot at a time, and ReleaseClaim frees it.
func TestClaimGenerationIsAtomic(t *testing.T) {
	eng, st, _ := testEngine(t, &scriptedProvider{}, &fakeMCP{})
	chatID := newTestChat(t, st)

	if err := eng.ClaimGeneration(chatID); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := eng.ClaimGeneration(chatID); !errors.Is(err, ErrGenerationActive) {
		t.Fatalf("second claim should conflict, got %v", err)
	}
	eng.ReleaseClaim(chatID)
	if err := eng.ClaimGeneration(chatID); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	eng.ReleaseClaim(chatID)
}

// TestConcurrentStartGenerationOnlyOneWins hammers StartGeneration from
// several goroutines against a provider that blocks: exactly one may win.
func TestConcurrentStartGenerationOnlyOneWins(t *testing.T) {
	prov := &blockingProvider{prefix: "x"}
	eng, st, _ := testEngine(t, prov, &fakeMCP{})
	chatID := newTestChat(t, st)
	if _, err := st.CreateMessage(context.Background(), store.NewMessageParams{
		ChatID: chatID, Role: store.RoleUser, Status: store.StatusComplete, Content: "go",
	}); err != nil {
		t.Fatal(err)
	}

	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := eng.startGeneration(context.Background(), chatID)
			errs <- err
		}()
	}
	successes, conflicts := 0, 0
	for i := 0; i < n; i++ {
		err := <-errs
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrGenerationActive):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if successes != 1 || conflicts != n-1 {
		t.Fatalf("successes=%d conflicts=%d, want 1/%d", successes, conflicts, n-1)
	}
	eng.StopGeneration(chatID)
}

// TestFlushNeverOverwritesFinalContent is the regression guard for the
// flush-vs-finalize race: with a tiny flush interval the flusher races the
// finalize; the persisted content must always end up exactly the final text.
func TestFlushNeverOverwritesFinalContent(t *testing.T) {
	var script []provider.StreamEvent
	full := ""
	for i := 0; i < 40; i++ {
		tok := "token "
		script = append(script, provider.StreamEvent{Kind: provider.EventTextDelta, Text: tok})
		full += tok
	}
	script = append(script, provider.StreamEvent{Kind: provider.EventDone, Finish: "stop"})
	prov := &scriptedProvider{scripts: [][]provider.StreamEvent{script}}
	eng, st, _ := testEngine(t, prov, &fakeMCP{})
	eng.flushInterval = time.Millisecond // race the flusher against finalize
	chatID := newTestChat(t, st)
	if _, err := st.CreateMessage(context.Background(), store.NewMessageParams{
		ChatID: chatID, Role: store.RoleUser, Status: store.StatusComplete, Content: "hi",
	}); err != nil {
		t.Fatal(err)
	}
	am, err := eng.startGeneration(context.Background(), chatID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "completion", 5*time.Second, func() bool {
		m, err := st.GetMessage(context.Background(), am.ID)
		return err == nil && m.Status == store.StatusComplete
	})
	// Let several (would-be stale) flush intervals pass; content must stay final.
	time.Sleep(20 * time.Millisecond)
	m, err := st.GetMessage(context.Background(), am.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Content != full {
		t.Fatalf("content = %q (len %d), want len %d", m.Content, len(m.Content), len(full))
	}
}

// TestStartSurvivesCanceledRequestContext guards B6 at the start boundary:
// the caller ctx is typically the HTTP request context; a client disconnect
// between claim and assistant-message creation must not leave the user
// message unanswered.
func TestStartSurvivesCanceledRequestContext(t *testing.T) {
	prov := &scriptedProvider{scripts: [][]provider.StreamEvent{{
		{Kind: provider.EventTextDelta, Text: "ok"},
		{Kind: provider.EventDone, Finish: "stop"},
	}}}
	eng, st, _ := testEngine(t, prov, &fakeMCP{})
	chatID := newTestChat(t, st)
	if _, err := st.CreateMessage(context.Background(), store.NewMessageParams{
		ChatID: chatID, Role: store.RoleUser, Status: store.StatusComplete, Content: "hi",
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // client already gone
	am, err := eng.startGeneration(ctx, chatID)
	if err != nil {
		t.Fatalf("start with canceled request ctx: %v", err)
	}
	waitFor(t, "generation complete", 5*time.Second, func() bool {
		m, err := st.GetMessage(context.Background(), am.ID)
		return err == nil && m.Status == store.StatusComplete
	})
	m, _ := st.GetMessage(context.Background(), am.ID)
	if m.Content != "ok" {
		t.Fatalf("content = %q", m.Content)
	}
}

// TestHubEpochChangesAcrossRecreation verifies the seq-reset signal: when a
// hub is pruned and recreated, the new incarnation carries a fresh epoch so
// clients reset their dedupe baseline (a stale lastSeq would otherwise
// silently drop every event of the next generation).
func TestHubEpochChangesAcrossRecreation(t *testing.T) {
	eng, st, _ := testEngine(t, &scriptedProvider{}, &fakeMCP{})
	chatID := newTestChat(t, st)

	ch, unsub := eng.Subscribe(chatID, 0)
	ev := <-ch // idle: no generation on this chat
	if ev.Type != "idle" || ev.Epoch == "" {
		t.Fatalf("want idle with epoch, got %+v", ev)
	}
	first := ev.Epoch
	unsub() // last subscriber gone + no generation -> hub pruned

	ch2, unsub2 := eng.Subscribe(chatID, 0)
	defer unsub2()
	ev2 := <-ch2
	if ev2.Type != "idle" {
		t.Fatalf("want idle, got %+v", ev2)
	}
	if ev2.Epoch == first {
		t.Fatalf("epoch unchanged across hub recreation: %q", first)
	}
}

// TestToolCreatedAttachment runs the tool loop against the REAL integrated
// catalog (create_text_file) and verifies the file lands as an attachment
// linked to the assistant message, and that attachment_created is published
// into the replay buffer with the meta.
func TestToolCreatedAttachment(t *testing.T) {
	args := `{"filename":"lorem.txt","content":"lorem ipsum"}`
	prov := &scriptedProvider{
		scripts: [][]provider.StreamEvent{
			{
				{Kind: provider.EventToolCallDone, CallID: "call_1", Name: "create_text_file", Args: args},
				{Kind: provider.EventDone, Finish: "tool_calls"},
			},
			{
				{Kind: provider.EventTextDelta, Text: "Here is your file."},
				{Kind: provider.EventDone, Finish: "stop"},
			},
		},
	}
	// Engine wired with the real integrated registry; the store is attached
	// after testEngine creates it (Builtin needs the same store instance).
	eng, st, _ := testEngine(t, prov, &fakeMCP{})
	eng.catalog = tools.Builtin(st, nil)
	chatID := newTestChat(t, st)
	if _, err := st.CreateMessage(context.Background(), store.NewMessageParams{
		ChatID: chatID, Role: store.RoleUser, Status: store.StatusComplete, Content: "make lorem.txt",
	}); err != nil {
		t.Fatal(err)
	}

	// Subscribe BEFORE starting so every event is captured live.
	ch, unsub := eng.Subscribe(chatID, -1)
	defer unsub()

	am, err := eng.startGeneration(context.Background(), chatID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "generation complete", 5*time.Second, func() bool {
		m, err := st.GetMessage(context.Background(), am.ID)
		return err == nil && m.Status == store.StatusComplete
	})

	// Attachment persisted, linked to the assistant message, correct content.
	metas, err := st.ListAttachmentsByMessage(context.Background(), am.ID)
	if err != nil || len(metas) != 1 {
		t.Fatalf("want 1 attachment on assistant message, got %d (err=%v)", len(metas), err)
	}
	meta := metas[0]
	if meta.Filename != "lorem.txt" || meta.Kind != "text" || meta.ChatID != chatID {
		t.Fatalf("attachment meta wrong: %+v", meta)
	}
	att, err := st.GetAttachment(context.Background(), meta.ID)
	if err != nil || string(att.Data) != "lorem ipsum" {
		t.Fatalf("attachment blob wrong: %q err=%v", att.Data, err)
	}

	// Tool result mentions the filename (what the model sees).
	msgs, _ := st.ListMessages(context.Background(), chatID)
	var toolResult string
	for _, m := range msgs {
		if m.Role == store.RoleTool {
			toolResult = m.Content
		}
	}
	if !strings.Contains(toolResult, "lorem.txt") {
		t.Fatalf("tool result does not mention file: %q", toolResult)
	}

	// attachment_created was published with the meta and the message id.
	sawAttachment := false
	timeout := time.After(2 * time.Second)
	for !sawAttachment {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("stream closed before attachment_created")
			}
			if ev.Type == "attachment_created" {
				if ev.Attachment == nil || ev.Attachment.ID != meta.ID {
					t.Fatalf("attachment_created payload wrong: %+v", ev.Attachment)
				}
				if ev.MessageID != am.ID {
					t.Fatalf("attachment_created message_id = %q, want %q", ev.MessageID, am.ID)
				}
				sawAttachment = true
			}
		case <-timeout:
			t.Fatal("attachment_created never published")
		}
	}
}

// TestBroadcastChatUpdatedReachesGlobalWithoutHub: a title broadcast for a
// chat with no hub (idle, no subscribers) must still fan out to global
// subscribers — other tabs' sidebars rely on it.
func TestBroadcastChatUpdatedReachesGlobalWithoutHub(t *testing.T) {
	eng, st, _ := testEngine(t, &scriptedProvider{}, &fakeMCP{})
	ch, unsub := eng.SubscribeGlobal()
	defer unsub()
	ev := <-ch // generating_snapshot on subscribe
	if ev.Type != "generating_snapshot" {
		t.Fatalf("want generating_snapshot first, got %+v", ev)
	}
	chat, err := st.CreateChat(context.Background(), "m", store.GenParams{}, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if eng.hubIfExists(chat.ID) != nil {
		t.Fatal("precondition broken: chat should have no hub")
	}
	eng.BroadcastChatUpdated(chat.ID, "Renamed While Idle")
	select {
	case ev := <-ch:
		if ev.Type != "chat_updated" || ev.Title != "Renamed While Idle" || ev.ChatID != chat.ID {
			t.Fatalf("unexpected global event: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("chat_updated for hub-less chat never reached the global stream")
	}
}

// TestPublishConfigChangedReachesGlobal: the MCP catalog is rebuilt
// asynchronously after a config save; clients rely on the config_changed
// global event to refetch /api/config (tools menu) without a page reload.
func TestPublishConfigChangedReachesGlobal(t *testing.T) {
	eng, _, _ := testEngine(t, &scriptedProvider{}, &fakeMCP{})
	ch, unsub := eng.SubscribeGlobal()
	defer unsub()
	ev := <-ch // generating_snapshot on subscribe
	if ev.Type != "generating_snapshot" {
		t.Fatalf("want generating_snapshot first, got %+v", ev)
	}
	eng.PublishConfigChanged()
	select {
	case ev := <-ch:
		if ev.Type != "config_changed" {
			t.Fatalf("unexpected global event: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("config_changed never reached the global stream")
	}
}
