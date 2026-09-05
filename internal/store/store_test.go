package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"chattoneko/internal/db"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	sqlDB, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return NewStore(sqlDB)
}

func newChat(t *testing.T, s *Store) *Chat {
	t.Helper()
	chat, err := s.CreateChat(context.Background(), "m", GenParams{ReasoningEffort: "low"}, map[string]bool{"t1": true})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	return chat
}

func TestChatCRUDRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	chat := newChat(t, s)

	got, err := s.GetChat(ctx, chat.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Model != "m" || got.Params.ReasoningEffort != "low" || !got.Tools["t1"] {
		t.Fatalf("settings round-trip failed: %+v", got)
	}

	if err := s.UpdateChatTitle(ctx, chat.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateChatSettings(ctx, chat.ID, "m2", GenParams{}, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetChat(ctx, chat.ID)
	if got.Title != "hello" || got.Model != "m2" {
		t.Fatalf("updates not persisted: %+v", got)
	}

	if ok, err := s.ChatExists(ctx, chat.ID); err != nil || !ok {
		t.Fatal("chat should exist")
	}
	if err := s.DeleteChat(ctx, chat.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetChat(ctx, chat.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted chat: err = %v", err)
	}
}

func TestGetChatNotFoundMapsErrNotFound(t *testing.T) {
	s := testStore(t)
	if _, err := s.GetChat(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.GetMessage(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.GetAttachment(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestListChatsCursorPagination(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		newChat(t, s)
	}
	// (updated_at, id) ties are fine: the compound cursor disambiguates.
	page1, err := s.ListChats(ctx, 2, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d", len(page1))
	}
	last := page1[len(page1)-1]
	page2, err := s.ListChats(ctx, 2, last.UpdatedAt, last.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 {
		t.Fatalf("page2 len = %d", len(page2))
	}
	// Pages must not overlap.
	seen := map[string]bool{}
	for _, c := range append(page1, page2...) {
		if seen[c.ID] {
			t.Fatalf("duplicate chat across pages: %s", c.ID)
		}
		seen[c.ID] = true
	}
	// Most recent first.
	if page1[0].UpdatedAt < page1[1].UpdatedAt {
		t.Fatal("not ordered by updated_at desc")
	}
}

func TestSearchChats(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c1 := newChat(t, s)
	c2 := newChat(t, s)
	if err := s.UpdateChatTitle(ctx, c1.ID, "golang tips"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMessage(ctx, NewMessageParams{ChatID: c2.ID, Role: RoleUser, Status: StatusComplete, Content: "tell me about GOLANG"}); err != nil {
		t.Fatal(err)
	}

	// Title match.
	found, err := s.SearchChats(ctx, "golang", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("want 2 matches (title + content), got %d", len(found))
	}
	// Content-only match.
	found, err = s.SearchChats(ctx, "tell me", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].ID != c2.ID {
		t.Fatalf("content search: %+v", found)
	}
	// No match.
	found, err = s.SearchChats(ctx, "zzzznothing", 50)
	if err != nil || len(found) != 0 {
		t.Fatalf("no-match search: %v %+v", err, found)
	}
}

func TestMessagesToolCallsAttachmentsRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	chat := newChat(t, s)

	um, err := s.CreateMessage(ctx, NewMessageParams{ChatID: chat.ID, Role: RoleUser, Status: StatusComplete, Content: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	am, err := s.CreateMessage(ctx, NewMessageParams{ChatID: chat.ID, Role: RoleAssistant, Status: StatusComplete, Content: "answer", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if um.Seq == 0 || am.Seq <= um.Seq {
		t.Fatalf("seq assignment wrong: user=%d assistant=%d", um.Seq, am.Seq)
	}

	tc, err := s.CreateToolCall(ctx, am.ID, "call_1", "echo", `{"a":1}`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tc.ID == "" || tc.ProviderCallID != "call_1" {
		t.Fatalf("tool call meta wrong: %+v", tc)
	}

	meta, err := s.CreateAttachment(ctx, chat.ID, "notes.md", "text", "text/markdown", 5, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID == "" || meta.MessageID != "" {
		t.Fatalf("attachment should be orphan: %+v", meta)
	}
	if err := s.LinkAttachmentToMessage(ctx, meta.ID, um.ID, chat.ID); err != nil {
		t.Fatal(err)
	}

	msgs, err := s.ListMessages(ctx, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	if len(msgs[0].Attachments) != 1 || msgs[0].Attachments[0].Filename != "notes.md" {
		t.Fatalf("attachment not grouped onto user message: %+v", msgs[0].Attachments)
	}
	if len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].Name != "echo" {
		t.Fatalf("tool call not grouped onto assistant message: %+v", msgs[1].ToolCalls)
	}

	// Dangling detection: call_1 has no tool result yet.
	dangling, err := s.ListDanglingToolCalls(ctx, am.ID)
	if err != nil || len(dangling) != 1 {
		t.Fatalf("dangling = %v %v", dangling, err)
	}
	if _, err := s.CreateMessage(ctx, NewMessageParams{
		ChatID: chat.ID, Role: RoleTool, Status: StatusComplete, Content: "ok", ToolCallID: "call_1", Name: "echo",
	}); err != nil {
		t.Fatal(err)
	}
	dangling, err = s.ListDanglingToolCalls(ctx, am.ID)
	if err != nil || len(dangling) != 0 {
		t.Fatalf("still dangling after result: %v %v", dangling, err)
	}

	// Finalize + usage.
	if err := s.FinalizeMessage(ctx, am.ID, StatusComplete, "", "answer", "thought"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateMessageUsage(ctx, am.ID, 10, 20, 30); err != nil {
		t.Fatal(err)
	}
	m, _ := s.GetMessage(ctx, am.ID)
	if m.Status != StatusComplete || m.Reasoning != "thought" || m.PromptTokens != 10 || m.DurationMs != 30 {
		t.Fatalf("finalize/usage wrong: %+v", m)
	}
	prompt, completion, err := s.ChatTokenTotals(ctx, chat.ID)
	if err != nil || prompt != 10 || completion != 20 {
		t.Fatalf("totals = %d/%d %v", prompt, completion, err)
	}
}

func TestLastAssistantMessageAndDeletingFromSeq(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	chat := newChat(t, s)
	if _, err := s.LastAssistantMessage(ctx, chat.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty chat: err = %v", err)
	}
	u1, _ := s.CreateMessage(ctx, NewMessageParams{ChatID: chat.ID, Role: RoleUser, Status: StatusComplete, Content: "a"})
	a1, _ := s.CreateMessage(ctx, NewMessageParams{ChatID: chat.ID, Role: RoleAssistant, Status: StatusComplete, Content: "b"})
	if _, err := s.CreateMessage(ctx, NewMessageParams{ChatID: chat.ID, Role: RoleUser, Status: StatusComplete, Content: "c"}); err != nil {
		t.Fatal(err)
	}

	last, err := s.LastAssistantMessage(ctx, chat.ID)
	if err != nil || last.ID != a1.ID {
		t.Fatalf("last assistant = %+v %v", last, err)
	}
	// Regenerate path: delete from the assistant message onward.
	if err := s.DeleteMessagesFromSeq(ctx, chat.ID, a1.Seq); err != nil {
		t.Fatal(err)
	}
	msgs, _ := s.ListMessages(ctx, chat.ID)
	if len(msgs) != 1 || msgs[0].ID != u1.ID {
		t.Fatalf("after delete-from-seq: %d messages", len(msgs))
	}
	// Edit path: delete strictly after a seq.
	if _, err := s.CreateMessage(ctx, NewMessageParams{ChatID: chat.ID, Role: RoleUser, Status: StatusComplete, Content: "c"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMessagesAfterSeq(ctx, chat.ID, u1.Seq); err != nil {
		t.Fatal(err)
	}
	msgs, _ = s.ListMessages(ctx, chat.ID)
	if len(msgs) != 1 || msgs[0].ID != u1.ID {
		t.Fatalf("after delete-after-seq: %d messages", len(msgs))
	}
}

func TestListGeneratingMessages(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	chat := newChat(t, s)
	m, err := s.CreateMessage(ctx, NewMessageParams{ChatID: chat.ID, Role: RoleAssistant, Status: StatusGenerating})
	if err != nil {
		t.Fatal(err)
	}
	gen, err := s.ListGeneratingMessages(ctx)
	if err != nil || len(gen) != 1 || gen[0].ID != m.ID {
		t.Fatalf("generating = %+v %v", gen, err)
	}
	if err := s.FinalizeMessage(ctx, m.ID, StatusFailed, "x", "", ""); err != nil {
		t.Fatal(err)
	}
	gen, _ = s.ListGeneratingMessages(ctx)
	if len(gen) != 0 {
		t.Fatalf("still generating: %+v", gen)
	}
}

func TestOrphanAttachmentSweep(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	chat := newChat(t, s)
	old, err := s.CreateAttachment(ctx, chat.ID, "old.txt", "text", "text/plain", 1, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := s.CreateAttachment(ctx, chat.ID, "fresh.txt", "text", "text/plain", 1, []byte("y"))
	if err != nil {
		t.Fatal(err)
	}
	// Link `fresh` to a message so only `old` is an orphan.
	m, _ := s.CreateMessage(ctx, NewMessageParams{ChatID: chat.ID, Role: RoleUser, Status: StatusComplete, Content: "hi"})
	if err := s.LinkAttachmentToMessage(ctx, fresh.ID, m.ID, chat.ID); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().UnixMilli() + 1000 // everything here is "old"
	if err := s.DeleteOrphanAttachments(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAttachment(ctx, old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("orphan not deleted: %v", err)
	}
	if _, err := s.GetAttachment(ctx, fresh.ID); err != nil {
		t.Fatalf("linked attachment deleted: %v", err)
	}
}

func TestEmptyChatSweep(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	empty := newChat(t, s)
	busy := newChat(t, s)
	if _, err := s.CreateMessage(ctx, NewMessageParams{ChatID: busy.ID, Role: RoleUser, Status: StatusComplete, Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	old, err := s.ListEmptyChatsOlderThan(ctx, time.Now().UnixMilli()+1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != 1 || old[0].ID != empty.ID {
		t.Fatalf("empty chats = %+v", old)
	}
}

func TestTitleGenerationQueries(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	titleless := func() []string {
		ids, err := s.ListChatsNeedingTitle(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	contains := func(ids []string, id string) bool {
		for _, x := range ids {
			if x == id {
				return true
			}
		}
		return false
	}

	// A brand-new chat is a title-task candidate (title_generated defaults to 0).
	chat := newChat(t, s)
	if !contains(titleless(), chat.ID) {
		t.Fatal("new chat not listed as needing a title")
	}

	// SetGeneratedTitle applies while the flag is 0, then never again.
	ok, err := s.SetGeneratedTitle(ctx, chat.ID, "AI Title")
	if err != nil || !ok {
		t.Fatalf("first SetGeneratedTitle: ok=%v err=%v", ok, err)
	}
	got, _ := s.GetChat(ctx, chat.ID)
	if got.Title != "AI Title" {
		t.Fatalf("title = %q", got.Title)
	}
	ok, err = s.SetGeneratedTitle(ctx, chat.ID, "Second Title")
	if err != nil || ok {
		t.Fatalf("second SetGeneratedTitle must be a no-op: ok=%v err=%v", ok, err)
	}
	if contains(titleless(), chat.ID) {
		t.Fatal("chat still a candidate after generated title")
	}

	// A manual rename flips the flag too, so the task never overwrites it.
	renamed := newChat(t, s)
	if err := s.UpdateChatTitle(ctx, renamed.ID, "My Title"); err != nil {
		t.Fatal(err)
	}
	if contains(titleless(), renamed.ID) {
		t.Fatal("renamed chat still listed as needing a title")
	}
	ok, err = s.SetGeneratedTitle(ctx, renamed.ID, "AI Overwrite")
	if err != nil || ok {
		t.Fatalf("SetGeneratedTitle after rename must be a no-op: ok=%v err=%v", ok, err)
	}
	got, _ = s.GetChat(ctx, renamed.ID)
	if got.Title != "My Title" {
		t.Fatalf("rename overwritten: title = %q", got.Title)
	}

	// MarkTitleGenerated drops the candidate without touching the title.
	kept := newChat(t, s)
	if err := s.MarkTitleGenerated(ctx, kept.ID); err != nil {
		t.Fatal(err)
	}
	if contains(titleless(), kept.ID) {
		t.Fatal("chat still a candidate after MarkTitleGenerated")
	}
	got, _ = s.GetChat(ctx, kept.ID)
	if got.Title != NewChatTitle {
		t.Fatalf("MarkTitleGenerated changed title to %q", got.Title)
	}
}

// TestConcurrentWritesSameChat verifies the store's concurrency safety: it
// is a stateless wrapper over database/sql, and SQLite WAL serializes the
// writes — concurrent message creation on one chat must all succeed and get
// distinct, monotonic global seqs. Runs with -race in CI.
func TestConcurrentWritesSameChat(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	chat := newChat(t, s)

	const n = 20
	errc := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			_, err := s.CreateMessage(ctx, NewMessageParams{
				ChatID:  chat.ID,
				Role:    RoleUser,
				Status:  StatusComplete,
				Content: fmt.Sprintf("msg %d", i),
			})
			errc <- err
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errc; err != nil {
			t.Fatalf("concurrent create %d: %v", i, err)
		}
	}
	msgs, err := s.ListMessages(ctx, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != n {
		t.Fatalf("messages = %d, want %d", len(msgs), n)
	}
	// seqs are strictly monotonic in list order.
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Seq <= msgs[i-1].Seq {
			t.Fatalf("seq not monotonic at %d: %d <= %d", i, msgs[i].Seq, msgs[i-1].Seq)
		}
	}
}

func TestFirstUserMessage(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	chat := newChat(t, s)

	// No messages at all: ErrNotFound.
	if _, err := s.FirstUserMessage(ctx, chat.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty chat: err = %v", err)
	}

	first, _ := s.CreateMessage(ctx, NewMessageParams{ChatID: chat.ID, Role: RoleUser, Status: StatusComplete, Content: "first"})
	if _, err := s.CreateMessage(ctx, NewMessageParams{ChatID: chat.ID, Role: RoleAssistant, Status: StatusComplete, Content: "reply"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMessage(ctx, NewMessageParams{ChatID: chat.ID, Role: RoleUser, Status: StatusComplete, Content: "second"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.FirstUserMessage(ctx, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != first.ID || got.Content != "first" {
		t.Fatalf("FirstUserMessage = %+v", got)
	}
}

func TestDeleteDanglingAttachments(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	chat := newChat(t, s)
	other := newChat(t, s)

	// Message that survives, with a linked attachment.
	kept, _ := s.CreateMessage(ctx, NewMessageParams{ChatID: chat.ID, Role: RoleUser, Status: StatusComplete, Content: "keep"})
	keptAtt, err := s.CreateLinkedAttachment(ctx, chat.ID, kept.ID, "keep.txt", "text", "text/plain", 1, []byte("k"))
	if err != nil {
		t.Fatal(err)
	}
	// Message that gets truncated away, with a linked attachment (the
	// tool-created-file case after regenerate/edit-resend).
	gone, _ := s.CreateMessage(ctx, NewMessageParams{ChatID: chat.ID, Role: RoleAssistant, Status: StatusComplete, Content: "gone"})
	goneAtt, err := s.CreateLinkedAttachment(ctx, chat.ID, gone.ID, "gone.txt", "text", "text/plain", 1, []byte("g"))
	if err != nil {
		t.Fatal(err)
	}
	// True orphan (upload not yet linked): must NOT be reaped here — the
	// send flow links it after this cleanup could run.
	orphan, err := s.CreateAttachment(ctx, chat.ID, "pending.txt", "text", "text/plain", 1, []byte("p"))
	if err != nil {
		t.Fatal(err)
	}
	// Same-shape dangling row in ANOTHER chat: out of scope for this call.
	otherGone, _ := s.CreateMessage(ctx, NewMessageParams{ChatID: other.ID, Role: RoleAssistant, Status: StatusComplete, Content: "x"})
	otherAtt, err := s.CreateLinkedAttachment(ctx, other.ID, otherGone.ID, "o.txt", "text", "text/plain", 1, []byte("o"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMessagesFromSeq(ctx, chat.ID, gone.Seq); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMessagesFromSeq(ctx, other.ID, otherGone.Seq); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteDanglingAttachments(ctx, chat.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAttachment(ctx, goneAtt.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dangling attachment not deleted: %v", err)
	}
	if _, err := s.GetAttachment(ctx, keptAtt.ID); err != nil {
		t.Fatalf("linked attachment wrongly deleted: %v", err)
	}
	if _, err := s.GetAttachment(ctx, orphan.ID); err != nil {
		t.Fatalf("unlinked (pending) attachment wrongly deleted: %v", err)
	}
	if _, err := s.GetAttachment(ctx, otherAtt.ID); err != nil {
		t.Fatalf("other chat's attachment wrongly deleted: %v", err)
	}
}
