package engine

import (
	"context"
	"testing"
	"time"

	"chattoneko/internal/provider"
	"chattoneko/internal/store"
)

// selectProvider blocks on model "slow" until canceled and answers model
// "fast" immediately — lets one engine run a stuck and a healthy generation
// concurrently in different chats.
type selectProvider struct{}

func (p *selectProvider) StreamChat(ctx context.Context, _ []provider.Message, _ []provider.Tool, gp provider.GenParams) (*provider.EventStream, error) {
	es := provider.NewEventStream(64, nil)
	go func() {
		if gp.Model == "slow" {
			es.Publish(provider.StreamEvent{Kind: provider.EventTextDelta, Text: "s"})
			<-ctx.Done()
			es.Finish(ctx.Err())
			return
		}
		es.Publish(provider.StreamEvent{Kind: provider.EventTextDelta, Text: "fast answer"})
		es.Publish(provider.StreamEvent{Kind: provider.EventDone, Finish: "stop"})
		es.Finish(nil)
	}()
	return es, nil
}

// TestSecondChatNotBlockedByFirst verifies a generation in chat B is not
// stalled by an in-flight (even stuck) generation in chat A — engine locks,
// the SQLite store and the fan-out must all stay per-chat.
func TestSecondChatNotBlockedByFirst(t *testing.T) {
	eng, st, _ := testEngine(t, &selectProvider{}, &fakeMCP{})

	newChat := func(model string) string {
		chat, err := st.CreateChat(context.Background(), model, store.GenParams{}, map[string]bool{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateMessage(context.Background(), store.NewMessageParams{
			ChatID: chat.ID, Role: store.RoleUser, Status: store.StatusComplete, Content: "hi",
		}); err != nil {
			t.Fatal(err)
		}
		return chat.ID
	}
	chatA, chatB := newChat("slow"), newChat("fast")

	amA, err := eng.startGeneration(context.Background(), chatA)
	if err != nil {
		t.Fatalf("start A: %v", err)
	}
	waitFor(t, "chat A streaming", 5*time.Second, func() bool {
		return eng.HasActiveGeneration(chatA)
	})

	amB, err := eng.startGeneration(context.Background(), chatB)
	if err != nil {
		t.Fatalf("start B: %v", err)
	}
	start := time.Now()
	waitFor(t, "chat B complete", 5*time.Second, func() bool {
		m, err := st.GetMessage(context.Background(), amB.ID)
		return err == nil && m.Status == store.StatusComplete
	})
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("chat B stalled for %v while chat A was generating", took)
	}
	m, _ := st.GetMessage(context.Background(), amB.ID)
	if m.Content != "fast answer" {
		t.Fatalf("B content = %q", m.Content)
	}

	// Unblock chat A and let it finalize before the test tears down the DB.
	eng.StopGeneration(chatA)
	waitFor(t, "chat A stopped", 5*time.Second, func() bool {
		m, err := st.GetMessage(context.Background(), amA.ID)
		return err == nil && m.Status == store.StatusStopped
	})
}
