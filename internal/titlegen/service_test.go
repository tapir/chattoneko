package titlegen

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"chattoneko/internal/attach"
	"chattoneko/internal/config"
	"chattoneko/internal/db"
	"chattoneko/internal/store"
)

// ---- harness ----

func testStore(t *testing.T) *store.Store {
	t.Helper()
	sqlDB, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return store.NewStore(sqlDB)
}

// fakeGen fakes the title LLM call; counts calls and can rename the chat
// mid-call to exercise the conditional-write race.
type fakeGen struct {
	textResult string
	textErr    error
	fileResult string
	fileErr    error
	textCalls  int
	fileCalls  int
	onText     func() // runs inside GenerateFromText (race injection)
}

func (f *fakeGen) GenerateFromText(_ context.Context, _ string) (string, error) {
	f.textCalls++
	if f.onText != nil {
		f.onText()
	}
	return f.textResult, f.textErr
}

func (f *fakeGen) GenerateFromFile(_ context.Context, _, _ string) (string, error) {
	f.fileCalls++
	return f.fileResult, f.fileErr
}

func newService(st *store.Store, gen generator) *Service {
	return &Service{
		store:    st,
		gen:      gen,
		hub:      NewHub(),
		interval: time.Millisecond,
		timeout:  5 * time.Second,
		batch:    16,
		retries:  map[string]*retryState{},
	}
}

func newChat(t *testing.T, st *store.Store) *store.Chat {
	t.Helper()
	chat, err := st.CreateChat(context.Background(), "m", store.GenParams{}, map[string]bool{})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	return chat
}

func addUserMessage(t *testing.T, st *store.Store, chatID, content string) *store.Message {
	t.Helper()
	msg, err := st.CreateMessage(context.Background(), store.NewMessageParams{
		ChatID:  chatID,
		Role:    store.RoleUser,
		Status:  store.StatusComplete,
		Content: content,
	})
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	return msg
}

func addAttachment(t *testing.T, st *store.Store, chatID, messageID, kind, filename string, data []byte) {
	t.Helper()
	meta, err := st.CreateAttachment(context.Background(), chatID, filename, kind, "text/plain", int64(len(data)), data)
	if err != nil {
		t.Fatalf("create attachment: %v", err)
	}
	if err := st.LinkAttachmentToMessage(context.Background(), meta.ID, messageID, chatID); err != nil {
		t.Fatalf("link attachment: %v", err)
	}
}

// needsTitle reports whether the chat is still a title-task candidate.
func needsTitle(t *testing.T, st *store.Store, chatID string) bool {
	t.Helper()
	ids, err := st.ListChatsNeedingTitle(context.Background(), 100)
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}
	for _, id := range ids {
		if id == chatID {
			return true
		}
	}
	return false
}

func chatTitle(t *testing.T, st *store.Store, chatID string) string {
	t.Helper()
	chat, err := st.GetChat(context.Background(), chatID)
	if err != nil {
		t.Fatalf("get chat: %v", err)
	}
	return chat.Title
}

// clearBackoff simulates the backoff window expiring.
func clearBackoff(s *Service, chatID string) {
	if r, ok := s.retries[chatID]; ok {
		r.next = time.Now().Add(-time.Hour)
	}
}

// ---- sweep behavior ----

func TestTitlesFromText(t *testing.T) {
	st := testStore(t)
	chat := newChat(t, st)
	addUserMessage(t, st, chat.ID, "how do I write a for loop in go?")

	gen := &fakeGen{textResult: "Go For Loops"}
	svc := newService(st, gen)

	events, unsub := svc.hub.Subscribe()
	defer unsub()

	svc.sweep(context.Background())

	if got := chatTitle(t, st, chat.ID); got != "Go For Loops" {
		t.Fatalf("title = %q", got)
	}
	if needsTitle(t, st, chat.ID) {
		t.Fatal("chat still a title candidate after generation")
	}
	if gen.textCalls != 1 {
		t.Fatalf("generator calls = %d, want 1", gen.textCalls)
	}
	select {
	case ev := <-events:
		if ev.ChatID != chat.ID || ev.Title != "Go For Loops" {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no title event published")
	}
}

func TestSkipsChatsWithoutMessages(t *testing.T) {
	st := testStore(t)
	chat := newChat(t, st) // no messages yet

	gen := &fakeGen{textResult: "X"}
	svc := newService(st, gen)
	svc.sweep(context.Background())

	if !needsTitle(t, st, chat.ID) {
		t.Fatal("empty chat must stay a candidate until its first message")
	}
	if gen.textCalls != 0 {
		t.Fatal("generator called for a chat with no messages")
	}
}

func TestTitlesFromTextFileAttachment(t *testing.T) {
	st := testStore(t)
	chat := newChat(t, st)
	msg := addUserMessage(t, st, chat.ID, "")
	addAttachment(t, st, chat.ID, msg.ID, attach.KindText, "server.log", []byte("2024-01-01 ERROR disk full on node 3"))

	gen := &fakeGen{fileResult: "Disk Full Errors"}
	svc := newService(st, gen)
	svc.sweep(context.Background())

	if got := chatTitle(t, st, chat.ID); got != "Disk Full Errors" {
		t.Fatalf("title = %q", got)
	}
	if gen.fileCalls != 1 || gen.textCalls != 0 {
		t.Fatalf("file calls = %d, text calls = %d", gen.fileCalls, gen.textCalls)
	}
}

func TestImageOnlyGetsFixedTitle(t *testing.T) {
	st := testStore(t)
	chat := newChat(t, st)
	msg := addUserMessage(t, st, chat.ID, "")
	addAttachment(t, st, chat.ID, msg.ID, attach.KindImage, "photo.png", []byte("fakepng"))

	gen := &fakeGen{}
	svc := newService(st, gen)
	svc.sweep(context.Background())

	if got := chatTitle(t, st, chat.ID); got != imageOnlyTitle {
		t.Fatalf("title = %q, want %q", got, imageOnlyTitle)
	}
	if gen.textCalls != 0 || gen.fileCalls != 0 {
		t.Fatal("generator must not run for image-only messages")
	}
	if needsTitle(t, st, chat.ID) {
		t.Fatal("image-only chat must be marked final")
	}
}

// Regression: with the provider/task model unconfigured the production
// generator must skip cleanly — a nil *client wrapped in the generator
// interface is non-nil and used to panic the whole sweep goroutine.
func TestSweepUnconfiguredDoesNotPanic(t *testing.T) {
	st := testStore(t)
	chat := newChat(t, st)
	addUserMessage(t, st, chat.ID, "hello")

	sqlDB, err := db.Open(t.TempDir() + "/cfg.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatal(err)
	}
	cfgs, err := config.NewStore(context.Background(), sqlDB)
	if err != nil {
		t.Fatal(err)
	}

	svc := New(st, cfgs) // production wiring, no provider configured
	svc.sweep(context.Background())

	if !needsTitle(t, st, chat.ID) {
		t.Fatal("chat must stay a candidate until the provider is configured")
	}
	if chatTitle(t, st, chat.ID) != store.NewChatTitle {
		t.Fatalf("title = %q", chatTitle(t, st, chat.ID))
	}
}

// A chat that leaves the candidate list while backing off (manual rename,
// deletion, mark-final) must have its retry state pruned — otherwise those
// entries leak for the server's whole lifetime.
func TestRetryStatePrunedWhenNotCandidate(t *testing.T) {
	st := testStore(t)
	chat := newChat(t, st)
	addUserMessage(t, st, chat.ID, "hello")

	gen := &fakeGen{textErr: errors.New("provider down")}
	svc := newService(st, gen)

	svc.sweep(context.Background()) // failure 1 → retry state exists
	if _, ok := svc.retries[chat.ID]; !ok {
		t.Fatal("no retry state after a failure")
	}
	// User renames while the chat is backing off: it leaves the candidate list.
	if err := st.UpdateChatTitle(context.Background(), chat.ID, "Mine"); err != nil {
		t.Fatal(err)
	}
	svc.sweep(context.Background())
	if _, ok := svc.retries[chat.ID]; ok {
		t.Fatal("stale retry state not pruned")
	}
}

func TestNothingUsableKeepsDefaultTitle(t *testing.T) {
	st := testStore(t)
	chat := newChat(t, st)
	addUserMessage(t, st, chat.ID, "   ") // whitespace-only, no attachments

	gen := &fakeGen{}
	svc := newService(st, gen)
	svc.sweep(context.Background())

	if got := chatTitle(t, st, chat.ID); got != store.NewChatTitle {
		t.Fatalf("title = %q, want %q", got, store.NewChatTitle)
	}
	if needsTitle(t, st, chat.ID) {
		t.Fatal("chat with unusable first message must be marked final")
	}
}

func TestRenameBeforeSweepWins(t *testing.T) {
	st := testStore(t)
	chat := newChat(t, st)
	addUserMessage(t, st, chat.ID, "hello")
	// User renames before the task runs: the task must not touch the chat.
	if err := st.UpdateChatTitle(context.Background(), chat.ID, "My Title"); err != nil {
		t.Fatal(err)
	}

	gen := &fakeGen{textResult: "AI Title"}
	svc := newService(st, gen)
	svc.sweep(context.Background())

	if got := chatTitle(t, st, chat.ID); got != "My Title" {
		t.Fatalf("title = %q, want user's rename", got)
	}
	if gen.textCalls != 0 {
		t.Fatal("generator ran for a chat the user already named")
	}
}

func TestRenameDuringGenerationWins(t *testing.T) {
	st := testStore(t)
	chat := newChat(t, st)
	addUserMessage(t, st, chat.ID, "hello")

	gen := &fakeGen{textResult: "AI Title"}
	// Inject the race: the user renames while the LLM call is in flight.
	gen.onText = func() {
		if err := st.UpdateChatTitle(context.Background(), chat.ID, "User Rename"); err != nil {
			t.Errorf("rename: %v", err)
		}
	}
	svc := newService(st, gen)

	events, unsub := svc.hub.Subscribe()
	defer unsub()

	svc.sweep(context.Background())

	if got := chatTitle(t, st, chat.ID); got != "User Rename" {
		t.Fatalf("title = %q, want user's rename to survive", got)
	}
	select {
	case ev := <-events:
		t.Fatalf("broadcast happened despite losing the race: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestTransientFailureRetriesWithBackoff(t *testing.T) {
	st := testStore(t)
	chat := newChat(t, st)
	addUserMessage(t, st, chat.ID, "hello")

	gen := &fakeGen{textErr: errors.New("provider down")}
	svc := newService(st, gen)

	svc.sweep(context.Background())
	if gen.textCalls != 1 {
		t.Fatalf("calls = %d", gen.textCalls)
	}
	if !needsTitle(t, st, chat.ID) {
		t.Fatal("transient failure must keep the chat a candidate")
	}
	// Immediate second sweep: the chat is backing off, no new call.
	svc.sweep(context.Background())
	if gen.textCalls != 1 {
		t.Fatal("backoff not honored: generator called while backing off")
	}
	// After the backoff expires, it retries — and can still succeed.
	gen.textErr = nil
	gen.textResult = "Recovered Title"
	clearBackoff(svc, chat.ID)
	svc.sweep(context.Background())
	if got := chatTitle(t, st, chat.ID); got != "Recovered Title" {
		t.Fatalf("title = %q after recovery", got)
	}
}

func TestGivesUpAfterMaxFailures(t *testing.T) {
	st := testStore(t)
	chat := newChat(t, st)
	addUserMessage(t, st, chat.ID, "hello")

	gen := &fakeGen{textErr: errors.New("provider down")}
	svc := newService(st, gen)

	for i := 0; i < maxFailures; i++ {
		clearBackoff(svc, chat.ID)
		svc.sweep(context.Background())
	}

	if needsTitle(t, st, chat.ID) {
		t.Fatal("chat must be marked final after repeated failures")
	}
	if got := chatTitle(t, st, chat.ID); got != store.NewChatTitle {
		t.Fatalf("title = %q, want %q", got, store.NewChatTitle)
	}
	if _, ok := svc.retries[chat.ID]; ok {
		t.Fatal("retry state not cleaned up after give-up")
	}
	// And it stops trying entirely.
	calls := gen.textCalls
	clearBackoff(svc, chat.ID)
	svc.sweep(context.Background())
	if gen.textCalls != calls {
		t.Fatal("generator called after give-up")
	}
}

func TestDeletedChatMidGenerationIsIgnored(t *testing.T) {
	st := testStore(t)
	chat := newChat(t, st)
	addUserMessage(t, st, chat.ID, "hello")

	gen := &fakeGen{textResult: "AI Title"}
	gen.onText = func() {
		if err := st.DeleteChat(context.Background(), chat.ID); err != nil {
			t.Errorf("delete: %v", err)
		}
	}
	svc := newService(st, gen)
	// Must not panic or broadcast; the conditional write hits 0 rows.
	svc.sweep(context.Background())
	if needsTitle(t, st, chat.ID) {
		t.Fatal("deleted chat still listed as candidate")
	}
}

// ---- sanitize / truncate ----

func TestSanitizeTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "Go For Loops", "Go For Loops"},
		{"surrounding quotes", `"Go For Loops"`, "Go For Loops"},
		{"backticks", "`go for loops`", "go for loops"},
		{"title prefix", "Title: Go For Loops", "Go For Loops"},
		{"title prefix lowercase", "title: go for loops", "go for loops"},
		{"first line only", "Go For Loops\nHere is why...", "Go For Loops"},
		{"trailing punctuation", "Go For Loops.", "Go For Loops"},
		{"trailing exclamation", "Go For Loops!", "Go For Loops"},
		{"collapses whitespace", "Go   For\t Loops", "Go For Loops"},
		{"control characters stripped", "Bell\x07Title\x00", "BellTitle"},
		{"unicode control stripped", "A\u009fB", "AB"},
		{"colon inside kept", "Go: For Loops", "Go: For Loops"},
		{"unicode kept", "日本語のタイトル", "日本語のタイトル"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sanitizeTitle(tc.in)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSanitizeTitleEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\n", `""`, "Title: ", "...", `"". `, "\x00\x07\x1b"} {
		if got, err := sanitizeTitle(in); err == nil {
			t.Fatalf("sanitizeTitle(%q) = %q, want error", in, got)
		}
	}
}

func TestSanitizeTitleLengthCap(t *testing.T) {
	long := strings.Repeat("word ", 30) // 150 chars
	got, err := sanitizeTitle(long)
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(got)) > maxTitleRunes {
		t.Fatalf("len = %d runes, want <= %d", len([]rune(got)), maxTitleRunes)
	}
	if strings.HasSuffix(got, " ") {
		t.Fatal("trailing space after truncation")
	}
}

// ---- task client (reasoning effort from the models table) ----

func ptr[T any](v T) *T { return &v }

// taskConfig builds a config store with provider + models configured and,
// when meta is non-nil, a stored metadata row for the task model. The raw
// DB is returned alongside for tests that need to break it.
func taskConfig(t *testing.T, taskModel string, meta *config.ModelMeta) (*config.Store, *sql.DB) {
	t.Helper()
	sqlDB, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	ctx := context.Background()
	cfgs, err := config.NewStore(ctx, sqlDB)
	if err != nil {
		t.Fatalf("config store: %v", err)
	}
	patch := config.Patch{
		Provider: &config.ProviderPatch{BaseURL: ptr("https://p.example/api"), APIKey: ptr("sk-test")},
		Models: &config.ModelsPatch{
			Whitelist:        &[]string{taskModel},
			DefaultChatModel: ptr(taskModel),
			DefaultTaskModel: ptr(taskModel),
		},
	}
	if meta != nil {
		patch.Models.Metas = &[]config.ModelMeta{*meta}
	}
	if _, err := cfgs.Update(ctx, patch); err != nil {
		t.Fatalf("update config: %v", err)
	}
	return cfgs, sqlDB
}

func TestTaskClientUsesDatabaseDefaultEffort(t *testing.T) {
	meta := config.DefaultModelMeta("task-m")
	meta.ReasoningEfforts = []string{"low", "high"}
	meta.ReasoningDefault = "high"
	cfgs, _ := taskConfig(t, "task-m", &meta)

	svc := New(testStore(t), cfgs)
	cli := svc.taskClient(context.Background())
	if cli == nil {
		t.Fatal("task client nil despite provider/task model configured")
	}
	if cli.effort != "high" {
		t.Fatalf("effort = %q, want the stored default %q", cli.effort, "high")
	}
}

func TestTaskClientDefaultEffortWithoutStoredRow(t *testing.T) {
	// No metadata row: ModelMetas applies the spec defaults (default medium).
	cfgs, _ := taskConfig(t, "task-m", nil)

	svc := New(testStore(t), cfgs)
	cli := svc.taskClient(context.Background())
	if cli == nil {
		t.Fatal("task client nil")
	}
	if cli.effort != config.DefaultModelMeta("task-m").ReasoningDefault {
		t.Fatalf("effort = %q, want spec default", cli.effort)
	}
}

func TestTaskClientRebuildsOnEffortChange(t *testing.T) {
	meta := config.DefaultModelMeta("task-m")
	meta.ReasoningDefault = "low"
	cfgs, _ := taskConfig(t, "task-m", &meta)

	svc := New(testStore(t), cfgs)
	ctx := context.Background()
	first := svc.taskClient(ctx)
	if first == nil || first.effort != "low" {
		t.Fatalf("first client = %+v", first)
	}
	// Same settings: the cached client is reused.
	if got := svc.taskClient(ctx); got != first {
		t.Fatal("client rebuilt without config change")
	}
	// Admin changes the stored default effort: the client must be rebuilt.
	meta.ReasoningDefault = "high"
	if err := cfgs.UpsertModelMetas(ctx, []config.ModelMeta{meta}); err != nil {
		t.Fatalf("upsert metas: %v", err)
	}
	second := svc.taskClient(ctx)
	if second == first {
		t.Fatal("client not rebuilt after effort change")
	}
	if second.effort != "high" {
		t.Fatalf("effort = %q, want high", second.effort)
	}
}

func TestTaskClientFallsBackOnMetaLoadFailure(t *testing.T) {
	meta := config.DefaultModelMeta("task-m")
	cfgs, sqlDB := taskConfig(t, "task-m", &meta)

	svc := New(testStore(t), cfgs)
	// Break the models table; metadata reads fail but titles must not stall.
	if _, err := sqlDB.Exec(`DROP TABLE models`); err != nil {
		t.Fatalf("drop models table: %v", err)
	}
	cli := svc.taskClient(context.Background())
	if cli == nil {
		t.Fatal("task client nil on metadata failure; want provider-default fallback")
	}
	if cli.effort != "" {
		t.Fatalf("effort = %q, want empty (provider default)", cli.effort)
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("hello", 10); got != "hello" {
		t.Fatalf("short string cut: %q", got)
	}
	// Multi-byte runes must never be split.
	s := strings.Repeat("日本", 10) // 20 runes
	got := truncateRunes(s, 7)
	if len([]rune(got)) > 7 {
		t.Fatalf("len = %d, want <= 7", len([]rune(got)))
	}
	// Word-boundary preference.
	if got := truncateRunes("alpha beta gamma delta", 12); got != "alpha beta" {
		t.Fatalf("word boundary cut = %q", got)
	}
}
