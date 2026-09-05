package api

import (
	"context"
	"strings"
	"testing"

	"chattoneko/internal/config"
	"chattoneko/internal/store"
)

func ptr[T any](v T) *T { return &v }

// setupVisionRoundtrip writes the vision model through PUT /api/setup and
// reads it back through GET /api/setup and GET /api/config.
func TestSetupVisionModelRoundtrip(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)

	// Whitelist the vision model and designate it.
	rec := ts.do(t, "PUT", "/api/setup", map[string]any{
		"models": map[string]any{
			"whitelist":            []string{"m", "vision-model"},
			"default_vision_model": "vision-model",
		},
	}, nil)
	if rec.Code != 200 {
		t.Fatalf("put setup: %d %s", rec.Code, rec.Body)
	}
	if got := ts.cfg.Get().Models.DefaultVisionModel; got != "vision-model" {
		t.Fatalf("stored vision model = %q", got)
	}

	// GET /api/setup exposes it.
	rec = ts.do(t, "GET", "/api/setup", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("get setup: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"default_vision_model":"vision-model"`) {
		t.Errorf("setup JSON missing vision model: %s", rec.Body)
	}

	// GET /api/config exposes it too (the composer hint reads it).
	rec = ts.do(t, "GET", "/api/config", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("get config: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"default_vision_model":"vision-model"`) {
		t.Errorf("config JSON missing vision model: %s", rec.Body)
	}
}

// The vision model must survive being dropped from the whitelist.
func TestSetupVisionModelClearedOffWhitelist(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)
	if _, err := ts.cfg.Update(context.Background(), config.Patch{
		Models: &config.ModelsPatch{
			Whitelist:          &[]string{"m", "vision-model"},
			DefaultVisionModel: ptr("vision-model"),
		},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Whitelist shrinks; vision model falls off.
	rec := ts.do(t, "PUT", "/api/setup", map[string]any{
		"models": map[string]any{"whitelist": []string{"m"}},
	}, nil)
	if rec.Code != 200 {
		t.Fatalf("put setup: %d %s", rec.Code, rec.Body)
	}
	if got := ts.cfg.Get().Models.DefaultVisionModel; got != "" {
		t.Fatalf("vision model = %q after whitelist removal, want empty", got)
	}
}

// description endpoint: 200 for described image attachments, 404 otherwise.
func TestGetAttachmentDescription(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)
	chatID := ts.createChat(t)

	img, err := ts.store.CreateAttachment(context.Background(), chatID, "cat.png", "image", "image/png", 4, []byte("PNGB"))
	if err != nil {
		t.Fatal(err)
	}
	// Not yet described → 404.
	rec := ts.do(t, "GET", "/api/attachments/"+img.ID+"/description", nil, nil)
	if rec.Code != 404 {
		t.Fatalf("undescribed image: %d", rec.Code)
	}
	if err := ts.store.SetAttachmentDescription(context.Background(), img.ID, "a fluffy cat"); err != nil {
		t.Fatal(err)
	}
	rec = ts.do(t, "GET", "/api/attachments/"+img.ID+"/description", nil, nil)
	if rec.Code != 200 || rec.Body.String() != "a fluffy cat" {
		t.Fatalf("description: %d %q", rec.Code, rec.Body.String())
	}

	// Text attachments never carry a description.
	txt, err := ts.store.CreateAttachment(context.Background(), chatID, "n.txt", "text", "text/plain", 5, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	rec = ts.do(t, "GET", "/api/attachments/"+txt.ID+"/description", nil, nil)
	if rec.Code != 404 {
		t.Fatalf("text attachment: %d", rec.Code)
	}

	// Unknown id → 404.
	rec = ts.do(t, "GET", "/api/attachments/nope/description", nil, nil)
	if rec.Code != 404 {
		t.Fatalf("unknown: %d", rec.Code)
	}
}

// Messages expose has_description on their attachment metas.
func TestChatMessagesExposeHasDescription(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)
	chatID := ts.createChat(t)

	img, err := ts.store.CreateAttachment(context.Background(), chatID, "cat.png", "image", "image/png", 4, []byte("PNGB"))
	if err != nil {
		t.Fatal(err)
	}
	msg, err := ts.store.CreateMessage(context.Background(), store.NewMessageParams{
		ChatID: chatID, Role: store.RoleUser, Status: store.StatusComplete, Content: "hi",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.store.LinkAttachmentToMessage(context.Background(), img.ID, msg.ID, chatID); err != nil {
		t.Fatal(err)
	}

	rec := ts.do(t, "GET", "/api/chats/"+chatID, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("get chat: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"has_description"`) {
		t.Errorf("undescribed image must not set has_description: %s", rec.Body)
	}
	if err := ts.store.SetAttachmentDescription(context.Background(), img.ID, "a fluffy cat"); err != nil {
		t.Fatal(err)
	}
	rec = ts.do(t, "GET", "/api/chats/"+chatID, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("get chat 2: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"has_description":true`) {
		t.Errorf("described image missing has_description: %s", rec.Body)
	}
}
