package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"chattoneko/internal/mcphub"
	"chattoneko/internal/provider"
	"chattoneko/internal/store"
)

// A generation whose slot is re-claimed mid-finalize must not publish its
// terminal events: once ag.done is set, ClaimGeneration succeeds while the
// finalizer is still writing, and the replacement generation installs as
// h.gen before the old one reaches its publishGen — so the old generation's
// status/done would be stamped with the NEW generation's message id and
// appended to its replay buffer, ending it on the client before it streamed.
//
// The window is widened: stopping the generation while the first of many tool
// calls runs leaves the rest dangling, and the finalizer writes one synthetic
// result per dangling call before it publishes — the claim lands inside that
// write sequence.
func TestSupersededMidFinalizeSuppressesTerminalEvents(t *testing.T) {
	const danglingCalls = 100
	const attempts = 3
	hits := 0

	eng, st, _ := testEngine(t, &scriptedProvider{}, &fakeMCP{})
	for attempt := 0; attempt < attempts; attempt++ {
		chatID := newTestChat(t, st)
		if _, err := st.CreateMessage(context.Background(), store.NewMessageParams{
			ChatID: chatID, Role: store.RoleUser, Status: store.StatusComplete, Content: "go",
		}); err != nil {
			t.Fatal(err)
		}
		ch, unsub := eng.Subscribe(chatID, 0)

		var round []provider.StreamEvent
		for i := 0; i < danglingCalls; i++ {
			round = append(round, provider.StreamEvent{
				Kind:   provider.EventToolCallDone,
				CallID: "c" + string(rune('0'+i%10)) + string(rune('0'+i/10)),
				Name:   "slow_tool", Args: "{}",
			})
		}
		eng.prov = &scriptedProvider{scripts: [][]provider.StreamEvent{
			append(round, provider.StreamEvent{Kind: provider.EventDone, Finish: "tool_calls"}),
			{{Kind: provider.EventDone, Finish: "stop"}},
		}}
		tool := &blockingTool{
			entry:    mcphub.Entry{Display: "slow_tool", Description: "d", Server: "s", DefaultEnabled: true},
			started:  make(chan string, danglingCalls),
			released: make(chan struct{}),
		}
		eng.catalog = tool

		amA, err := eng.startGeneration(context.Background(), chatID)
		if err != nil {
			t.Fatalf("start A: %v", err)
		}
		<-tool.started             // first call executing
		eng.StopGeneration(chatID) // finalize will owe danglingCalls-1 results
		close(tool.released)       // let the running call return
		eng.catalog = &fakeMCP{}
		eng.prov = &blockingProvider{prefix: "b"}

		// Claim the slot the moment it opens (genActive flips false when the
		// finalizer starts), then start B on a provider that stays streaming.
		var amB *store.Message
		deadline := time.Now().Add(5 * time.Second)
		for amB == nil && time.Now().Before(deadline) {
			if !eng.HasActiveGeneration(chatID) {
				if m, err := eng.startGeneration(context.Background(), chatID); err == nil {
					amB = m
				}
			} else {
				time.Sleep(100 * time.Microsecond)
			}
		}
		if amB == nil {
			t.Fatal("slot never became claimable during A's finalize")
		}

		// B streams one delta and blocks; while it is live, nothing may end
		// it. Drain events in FIFO order: once B's generation_started has been
		// read, no done/status may follow for either id — the superseded A's
		// terminal events are suppressed (a late done ends B on the client
		// whichever id it carries), and B's own only come after its stop
		// below. A terminal for A read BEFORE B's start is the no-race ordering
		// (A published first) and is fine.
		sawBStart := false
		sawTerminalA := false
		corrupt := ""
		drainUntil := time.Now().Add(300 * time.Millisecond)
		for time.Now().Before(drainUntil) && corrupt == "" {
			select {
			case ev, ok := <-ch:
				if !ok {
					t.Fatal("subscriber channel closed")
				}
				if ev.Type == "generation_started" && ev.MessageID == amB.ID {
					sawBStart = true
				}
				if ev.Type == "done" || ev.Type == "status" {
					if sawBStart {
						corrupt = ev.Type
					} else if ev.MessageID == amA.ID {
						sawTerminalA = true
					}
				}
			case <-time.After(20 * time.Millisecond):
			}
		}
		if corrupt != "" {
			t.Fatalf("attempt %d: terminal %s event reached the stream while generation B (message %s) was still live",
				attempt, corrupt, amB.ID)
		}
		if !sawTerminalA {
			hits++ // B claimed before A published: the supersede path ran
		}

		eng.StopGeneration(chatID)
		sawB := false
		deadline = time.Now().Add(5 * time.Second)
		for !sawB && time.Now().Before(deadline) {
			select {
			case ev, ok := <-ch:
				if !ok {
					t.Fatal("subscriber channel closed")
				}
				if ev.Type == "done" && ev.MessageID == amB.ID {
					sawB = true
				}
			case <-time.After(20 * time.Millisecond):
			}
		}
		if !sawB {
			t.Fatal("generation B's own done never arrived")
		}
		waitFor(t, "B stopped in store", 5*time.Second, func() bool {
			m, err := st.GetMessage(context.Background(), amB.ID)
			return err == nil && m.Status == store.StatusStopped
		})
		// Suppression must not skip persistence: A finalized in the store
		// even though its terminal events were dropped.
		waitFor(t, "A stopped in store", 5*time.Second, func() bool {
			m, err := st.GetMessage(context.Background(), amA.ID)
			return err == nil && m.Status == store.StatusStopped
		})
		// Every dangling call answered — the history stays replayable.
		synth := 0
		msgs, _ := st.ListMessages(context.Background(), chatID)
		for _, m := range msgs {
			if m.Role == store.RoleTool && strings.Contains(m.Content, "generation stopped by user") {
				synth++
			}
		}
		if synth != danglingCalls-1 {
			t.Fatalf("dangling calls synthesized: %d, want %d", synth, danglingCalls-1)
		}
		unsub()
	}
	if hits == 0 {
		t.Skipf("the slot was never re-claimed mid-finalize in %d attempts", attempts)
	}
	t.Logf("supersede window exercised in %d/%d attempts", hits, attempts)
}

// The wire guard, pinned without timing: publishing on behalf of a
// superseded generation must deliver nothing and must not touch the live
// generation's replay buffer.
func TestPublishGenBindsToCurrentGeneration(t *testing.T) {
	prov := &scriptedProvider{scripts: [][]provider.StreamEvent{{
		{Kind: provider.EventTextDelta, Text: "a"},
		{Kind: provider.EventDone, Finish: "stop"},
	}}}
	eng, st, _ := testEngine(t, prov, &fakeMCP{})
	chatID := newTestChat(t, st)
	if _, err := st.CreateMessage(context.Background(), store.NewMessageParams{
		ChatID: chatID, Role: store.RoleUser, Status: store.StatusComplete, Content: "hi",
	}); err != nil {
		t.Fatal(err)
	}
	amA, err := eng.startGeneration(context.Background(), chatID)
	if err != nil {
		t.Fatalf("start A: %v", err)
	}
	waitFor(t, "A done", 5*time.Second, func() bool { return !eng.HasActiveGeneration(chatID) })
	h := eng.hubFor(chatID)
	h.mu.Lock()
	oldA := h.gen
	h.mu.Unlock()
	if oldA == nil || oldA.messageID != amA.ID {
		t.Fatal("A's generation should still hold the hub during its grace period")
	}

	// B replaces A inside the grace period (a done generation is replaceable).
	eng.prov = &blockingProvider{prefix: "b"}
	amB, err := eng.startGeneration(context.Background(), chatID)
	if err != nil {
		t.Fatalf("start B: %v", err)
	}
	ch, unsub := eng.Subscribe(chatID, 0)
	defer unsub()
	if ev := <-ch; ev.Type != "generation_started" || ev.MessageID != amB.ID {
		t.Fatalf("want B's generation_started in replay, got %+v", ev)
	}

	h.mu.Lock()
	h.publishGen(oldA, WireEvent{Type: "status", Status: store.StatusStopped})
	h.mu.Unlock()

	select {
	case ev := <-ch:
		t.Fatalf("stale publish delivered: %+v", ev)
	default:
	}
	h.mu.Lock()
	bufLen := len(h.gen.buffer)
	h.mu.Unlock()
	if bufLen != 1 {
		t.Fatalf("stale publish leaked into the live generation's replay buffer (len %d)", bufLen)
	}

	eng.StopGeneration(chatID)
	waitFor(t, "B stopped in store", 5*time.Second, func() bool {
		m, err := st.GetMessage(context.Background(), amB.ID)
		return err == nil && m.Status == store.StatusStopped
	})
}
