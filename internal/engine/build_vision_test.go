package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"chattoneko/internal/attach"
	"chattoneko/internal/config"
	"chattoneko/internal/store"
)

// fakeDescriber stands in for the vision service in build tests.
type fakeDescriber struct {
	calls int
	text  string
	err   error
}

func (f *fakeDescriber) DescribeImage(_ context.Context, _ []byte, _ string) (string, error) {
	f.calls++
	return f.text, f.err
}

// setupVisionChat creates a chat whose user message carries one image
// attachment and returns the engine, the chat, the message history and the
// attachment meta. The chat model id is what modelSeesImages resolves.
func setupVisionChat(t *testing.T, desc *fakeDescriber, model string) (*Engine, *store.Chat, []*store.Message, store.AttachmentMeta) {
	t.Helper()
	e, st, cancel := testEngine(t, &scriptedProvider{}, &fakeMCP{})
	t.Cleanup(cancel)
	if desc != nil {
		e.vision = desc
	}

	chat, err := st.CreateChat(context.Background(), model, store.GenParams{}, nil)
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	msg, err := st.CreateMessage(context.Background(), store.NewMessageParams{
		ChatID:  chat.ID,
		Role:    store.RoleUser,
		Status:  store.StatusComplete,
		Content: "what is this?",
	})
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	att, err := st.CreateAttachment(context.Background(), chat.ID, "cat.png", attach.KindImage, "image/png", 4, []byte("PNGB"))
	if err != nil {
		t.Fatalf("create attachment: %v", err)
	}
	if err := st.LinkAttachmentToMessage(context.Background(), att.ID, msg.ID, chat.ID); err != nil {
		t.Fatalf("link attachment: %v", err)
	}
	msgs := []*store.Message{{
		ID:          msg.ID,
		Role:        store.RoleUser,
		Content:     "what is this?",
		Attachments: []store.AttachmentMeta{*att},
	}}
	return e, chat, msgs, *att
}

func TestBuildVisionCapableModelSendsImage(t *testing.T) {
	desc := &fakeDescriber{text: "a cat"}
	e, chat, msgs, _ := setupVisionChat(t, desc, "vision-model")
	// Mark the chat model as image-capable via stored metadata.
	if err := e.cfg.UpsertModelMetas(context.Background(), []config.ModelMeta{
		{ModelID: "vision-model", InputModality: []string{"text", "image"}},
	}); err != nil {
		t.Fatalf("upsert metas: %v", err)
	}

	out, err := e.buildProviderMessages(context.Background(), chat, msgs, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	user := out[len(out)-1]
	if len(user.Images) != 1 {
		t.Fatalf("expected image part, got %d images (content=%q)", len(user.Images), user.Content)
	}
	if strings.Contains(user.Content, "<file") {
		t.Errorf("vision-capable model got a description block: %q", user.Content)
	}
	if desc.calls != 0 {
		t.Errorf("describer called %d times for a vision-capable model", desc.calls)
	}
}

func TestBuildTextOnlyModelDescribesImage(t *testing.T) {
	desc := &fakeDescriber{text: "a fluffy orange cat"}
	e, chat, msgs, att := setupVisionChat(t, desc, "text-model")
	// No metadata row: text-model defaults to text-only input.

	out, err := e.buildProviderMessages(context.Background(), chat, msgs, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	user := out[len(out)-1]
	if len(user.Images) != 0 {
		t.Fatalf("text-only model got %d image parts", len(user.Images))
	}
	if !strings.Contains(user.Content, "<file name=\"cat.png\"") {
		t.Errorf("missing description file block: %q", user.Content)
	}
	if !strings.Contains(user.Content, "a fluffy orange cat") {
		t.Errorf("description text missing: %q", user.Content)
	}
	if desc.calls != 1 {
		t.Errorf("describer calls = %d, want 1", desc.calls)
	}
	// Persisted on the attachment row.
	full, err := e.store.GetAttachment(context.Background(), att.ID)
	if err != nil {
		t.Fatalf("get attachment: %v", err)
	}
	if full.Description != "a fluffy orange cat" {
		t.Errorf("persisted description = %q", full.Description)
	}

	// Second build (e.g. next tool-loop iteration) reuses the cache: the
	// describer is not called again even with a fresh attCache.
	if _, err := e.buildProviderMessages(context.Background(), chat, msgs, nil); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if desc.calls != 1 {
		t.Errorf("describer calls after rebuild = %d, want 1", desc.calls)
	}
}

func TestBuildTextOnlyModelUsesCachedDescription(t *testing.T) {
	desc := &fakeDescriber{text: "should not be used"}
	e, chat, msgs, att := setupVisionChat(t, desc, "text-model")
	if err := e.store.SetAttachmentDescription(context.Background(), att.ID, "a cached description"); err != nil {
		t.Fatalf("seed description: %v", err)
	}

	out, err := e.buildProviderMessages(context.Background(), chat, msgs, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	user := out[len(out)-1]
	if !strings.Contains(user.Content, "a cached description") {
		t.Errorf("cached description not used: %q", user.Content)
	}
	if desc.calls != 0 {
		t.Errorf("describer called despite cached description")
	}
}

func TestBuildDescriberFailureFallsBackToImage(t *testing.T) {
	desc := &fakeDescriber{err: errors.New("provider exploded")}
	e, chat, msgs, _ := setupVisionChat(t, desc, "text-model")

	out, err := e.buildProviderMessages(context.Background(), chat, msgs, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	user := out[len(out)-1]
	if len(user.Images) != 1 {
		t.Fatalf("fallback expected the image part, got %d images", len(user.Images))
	}
	if strings.Contains(user.Content, "<file") {
		t.Errorf("failed describer still produced a file block: %q", user.Content)
	}
}

func TestBuildNoDescriberSendsImage(t *testing.T) {
	e, chat, msgs, _ := setupVisionChat(t, nil, "text-model")

	out, err := e.buildProviderMessages(context.Background(), chat, msgs, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	user := out[len(out)-1]
	if len(user.Images) != 1 {
		t.Fatalf("expected image part with no describer, got %d", len(user.Images))
	}
}
