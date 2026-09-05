package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"chattoneko/internal/auth"
	"chattoneko/internal/config"
	"chattoneko/internal/db"
	"chattoneko/internal/engine"
	"chattoneko/internal/mcphub"
	"chattoneko/internal/provider"
	"chattoneko/internal/store"
	"chattoneko/internal/titlegen"
)

// testPassword is the plaintext password used by auth-enabled test servers
// (auth is env-var driven in production; tests inject it via TestStore).
const testPassword = "pw"

// ---- fakes ----

// quickProvider streams a short answer and finishes.
type quickProvider struct{}

func (quickProvider) StreamChat(_ context.Context, _ []provider.Message, _ []provider.Tool, _ provider.GenParams) (*provider.EventStream, error) {
	es := provider.NewEventStream(16, nil)
	go func() {
		es.Publish(provider.StreamEvent{Kind: provider.EventTextDelta, Text: "answer"})
		es.Publish(provider.StreamEvent{Kind: provider.EventDone, Finish: "stop"})
		es.Finish(nil)
	}()
	return es, nil
}

// blockingProvider streams a prefix then waits for cancellation.
type blockingProvider struct{}

func (blockingProvider) StreamChat(ctx context.Context, _ []provider.Message, _ []provider.Tool, _ provider.GenParams) (*provider.EventStream, error) {
	es := provider.NewEventStream(16, nil)
	go func() {
		es.Publish(provider.StreamEvent{Kind: provider.EventTextDelta, Text: "partial"})
		<-ctx.Done()
		es.Finish(ctx.Err())
	}()
	return es, nil
}

type emptyMCP struct{}

func (emptyMCP) Tools() []mcphub.Entry { return nil }
func (emptyMCP) Call(context.Context, string, string, mcphub.CallMeta) (string, bool, error) {
	return "", false, fmt.Errorf("no tools")
}

// ---- harness ----

type testServer struct {
	server *httptest.Server
	store  *store.Store
	db     *sql.DB
	cfg    *config.Store
	auth   *auth.Auth
	engine *engine.Engine
	titles *titlegen.Hub
}

var testStatic = fstest.MapFS{
	"index.html":     &fstest.MapFile{Data: []byte("<html>spa</html>")},
	"assets/app.js":  &fstest.MapFile{Data: []byte("console.log(1)")},
	"assets/app.css": &fstest.MapFile{Data: []byte("body{}")},
}

func newTestServer(t *testing.T, prov provider.Provider, authEnabled bool) *testServer {
	t.Helper()
	sqlDB, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := store.NewStore(sqlDB)
	cfgData := config.Config{
		Models: config.ModelsConfig{
			Whitelist:        []string{"m"},
			DefaultChatModel: "m",
		},
		Limits: config.LimitsConfig{UploadMaxFileBytes: 1 << 20, MaxToolIterations: 5, MCPCallTimeoutSeconds: 5},
	}
	if authEnabled {
		cfgData.Auth = config.AuthConfig{Enabled: true, Username: "admin", Password: testPassword}
	}
	cfg, err := config.TestStore(context.Background(), sqlDB, cfgData)
	if err != nil {
		t.Fatalf("config store: %v", err)
	}
	serverCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	eng := engine.New(serverCtx, st, prov, emptyMCP{}, cfg, nil)
	hub := mcphub.New(cfg)
	a := auth.New(cfg)
	titles := titlegen.NewHub()
	srv := New(cfg, st, a, eng, hub, titles, testStatic)
	ts := &testServer{
		server: httptest.NewServer(srv.Handler()),
		store:  st,
		db:     sqlDB,
		cfg:    cfg,
		auth:   a,
		engine: eng,
		titles: titles,
	}
	t.Cleanup(ts.server.Close)
	return ts
}

func (ts *testServer) do(t *testing.T, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, ts.server.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ts.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rec := httptest.NewRecorder()
	rec.Code = resp.StatusCode
	for k := range resp.Header {
		rec.Header()[k] = resp.Header[k]
	}
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	rec.Body = buf
	return rec
}

func (ts *testServer) createChat(t *testing.T) string {
	t.Helper()
	rec := ts.do(t, "POST", "/api/chats", map[string]any{"model": "m"}, nil)
	if rec.Code != 200 {
		t.Fatalf("create chat: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Chat store.Chat `json:"chat"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Chat.ID
}

func waitForMsg(t *testing.T, st *store.Store, msgID, status string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m, err := st.GetMessage(context.Background(), msgID)
		if err == nil && m.Status == status {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("message %s never reached status %s", msgID, status)
}

// ---- tests ----

func TestCreateChatValidation(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)

	// Malformed JSON → 400 (regression: was silently creating a default chat).
	req, _ := http.NewRequest("POST", ts.server.URL+"/api/chats", strings.NewReader("{nope"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed body: status = %d", resp.StatusCode)
	}

	// Empty body → default chat, 200.
	rec := ts.do(t, "POST", "/api/chats", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("empty body: %d %s", rec.Code, rec.Body)
	}
}

// Malformed or oversized request bodies are rejected: anything over the
// 1 MiB cap must surface as 413 (not a generic 400), short malformed
// bodies as 400.
func TestOversizedJSONBodyRejected(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)

	// Pure whitespace past the cap: the decoder reads to the limit without
	// ever seeing a JSON value, so the MaxBytesError surfaces as-is.
	body := strings.Repeat(" ", maxJSONBodyBytes+1024)
	req, _ := http.NewRequest("POST", ts.server.URL+"/api/chats", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: status = %d, want 413", resp.StatusCode)
	}

	// Short malformed body stays a plain 400.
	req, _ = http.NewRequest("POST", ts.server.URL+"/api/chats", strings.NewReader("{nope"))
	req.Header.Set("Content-Type", "application/json")
	resp, err = ts.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed body: status = %d, want 400", resp.StatusCode)
	}
}

// Attachments referenced at send time must exist AND belong to the target
// chat: the link query silently affects zero rows for foreign ids, so the
// handler validates up front and rejects with 400.
func TestSendMessageAttachmentOwnership(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)
	chatA := ts.createChat(t)
	chatB := ts.createChat(t)
	ctx := context.Background()

	foreign, err := ts.store.CreateAttachment(ctx, chatA, "a.png", "image", "image/png", 3, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}

	// Attachment of another chat → 400.
	rec := ts.do(t, "POST", "/api/chats/"+chatB+"/messages",
		map[string]any{"content": "hi", "attachment_ids": []string{foreign.ID}}, nil)
	if rec.Code != 400 {
		t.Fatalf("foreign attachment: %d %s", rec.Code, rec.Body)
	}
	// Unknown attachment id → 400.
	rec = ts.do(t, "POST", "/api/chats/"+chatB+"/messages",
		map[string]any{"content": "hi", "attachment_ids": []string{"nope"}}, nil)
	if rec.Code != 400 {
		t.Fatalf("unknown attachment: %d %s", rec.Code, rec.Body)
	}
	// The rejected sends must not have persisted a user message.
	msgs, err := ts.store.ListMessages(ctx, chatB)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("rejected sends persisted %d messages", len(msgs))
	}
	// Valid send with the owning chat's attachment → 200 and linked.
	rec = ts.do(t, "POST", "/api/chats/"+chatA+"/messages",
		map[string]any{"content": "look", "attachment_ids": []string{foreign.ID}}, nil)
	if rec.Code != 200 {
		t.Fatalf("valid send: %d %s", rec.Code, rec.Body)
	}
}

func TestSendMessageValidationAndConflict(t *testing.T) {
	ts := newTestServer(t, blockingProvider{}, false)
	chatID := ts.createChat(t)

	// Nonexistent chat → 404.
	rec := ts.do(t, "POST", "/api/chats/nope/messages", map[string]any{"content": "hi"}, nil)
	if rec.Code != 404 {
		t.Fatalf("nonexistent chat: %d", rec.Code)
	}
	// Empty content → 400.
	rec = ts.do(t, "POST", "/api/chats/"+chatID+"/messages", map[string]any{"content": "  "}, nil)
	if rec.Code != 400 {
		t.Fatalf("empty content: %d", rec.Code)
	}
	// OK send → 200, generation runs (blocking provider keeps it active).
	rec = ts.do(t, "POST", "/api/chats/"+chatID+"/messages", map[string]any{"content": "hi"}, nil)
	if rec.Code != 200 {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	var sendOut struct {
		ChatID             string `json:"chat_id"`
		AssistantMessageID string `json:"assistant_message_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sendOut); err != nil {
		t.Fatal(err)
	}
	// Second send while the first generation is active → 409, and crucially
	// NO extra unanswered user message may be persisted.
	rec = ts.do(t, "POST", "/api/chats/"+chatID+"/messages", map[string]any{"content": "second"}, nil)
	if rec.Code != 409 {
		t.Fatalf("concurrent send: %d %s", rec.Code, rec.Body)
	}
	msgs, err := ts.store.ListMessages(context.Background(), chatID)
	if err != nil {
		t.Fatal(err)
	}
	userMsgs := 0
	for _, m := range msgs {
		if m.Role == store.RoleUser {
			userMsgs++
		}
	}
	if userMsgs != 1 {
		t.Fatalf("user messages = %d, want 1 (rejected send must not persist)", userMsgs)
	}

	// Stop → 204; stopping again → 404.
	rec = ts.do(t, "DELETE", "/api/chats/"+chatID+"/generation", nil, nil)
	if rec.Code != 204 {
		t.Fatalf("stop: %d", rec.Code)
	}
	waitForMsg(t, ts.store, sendOut.AssistantMessageID, store.StatusStopped)
	rec = ts.do(t, "DELETE", "/api/chats/"+chatID+"/generation", nil, nil)
	if rec.Code != 404 {
		t.Fatalf("second stop: %d", rec.Code)
	}
}

func TestEditMessageClassification(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)
	chatID := ts.createChat(t)
	rec := ts.do(t, "POST", "/api/chats/"+chatID+"/messages", map[string]any{"content": "hi"}, nil)
	if rec.Code != 200 {
		t.Fatalf("send: %d", rec.Code)
	}
	var sendOut struct {
		UserMessage        *store.Message `json:"user_message"`
		AssistantMessageID string         `json:"assistant_message_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sendOut); err != nil {
		t.Fatal(err)
	}
	waitForMsg(t, ts.store, sendOut.AssistantMessageID, store.StatusComplete)

	// Unknown message id → 404.
	rec = ts.do(t, "PATCH", "/api/chats/"+chatID+"/messages/nope", map[string]any{"content": "x"}, nil)
	if rec.Code != 404 {
		t.Fatalf("unknown message: %d", rec.Code)
	}
	// Assistant message → 400 (only user messages editable).
	rec = ts.do(t, "PATCH", "/api/chats/"+chatID+"/messages/"+sendOut.AssistantMessageID, map[string]any{"content": "x"}, nil)
	if rec.Code != 400 {
		t.Fatalf("edit assistant: %d", rec.Code)
	}
	// Empty edit → 400.
	rec = ts.do(t, "PATCH", "/api/chats/"+chatID+"/messages/"+sendOut.UserMessage.ID, map[string]any{"content": ""}, nil)
	if rec.Code != 400 {
		t.Fatalf("empty edit: %d", rec.Code)
	}
	// Valid edit-and-resend → 200 and history truncated.
	rec = ts.do(t, "PATCH", "/api/chats/"+chatID+"/messages/"+sendOut.UserMessage.ID, map[string]any{"content": "edited"}, nil)
	if rec.Code != 200 {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body)
	}
	var editOut struct {
		AssistantMessageID string `json:"assistant_message_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &editOut); err != nil {
		t.Fatal(err)
	}
	waitForMsg(t, ts.store, editOut.AssistantMessageID, store.StatusComplete)
	m, err := ts.store.GetMessage(context.Background(), sendOut.UserMessage.ID)
	if err != nil || m.Content != "edited" {
		t.Fatalf("edited content = %q %v", m.Content, err)
	}
	// Old assistant message must be gone.
	if _, err := ts.store.GetMessage(context.Background(), sendOut.AssistantMessageID); err == nil {
		t.Fatal("old assistant message survived edit-and-resend")
	}
}

func TestEditMessageAttachments(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)
	chatID := ts.createChat(t)
	rec := ts.do(t, "POST", "/api/chats/"+chatID+"/messages", map[string]any{"content": "look"}, nil)
	if rec.Code != 200 {
		t.Fatalf("send: %d", rec.Code)
	}
	var sendOut struct {
		UserMessage        *store.Message `json:"user_message"`
		AssistantMessageID string         `json:"assistant_message_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sendOut); err != nil {
		t.Fatal(err)
	}
	waitForMsg(t, ts.store, sendOut.AssistantMessageID, store.StatusComplete)
	ctx := context.Background()

	// Link two attachments to the user message.
	a1, err := ts.store.CreateAttachment(ctx, chatID, "a.png", "image", "image/png", 3, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	a2, err := ts.store.CreateAttachment(ctx, chatID, "b.png", "image", "image/png", 3, []byte("y"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a1.ID, a2.ID} {
		if err := ts.store.LinkAttachmentToMessage(ctx, id, sendOut.UserMessage.ID, chatID); err != nil {
			t.Fatal(err)
		}
	}
	url := "/api/chats/" + chatID + "/messages/" + sendOut.UserMessage.ID

	// Keep-list referencing an attachment of another message → 400.
	other, err := ts.store.CreateAttachment(ctx, chatID, "c.png", "image", "image/png", 3, []byte("z"))
	if err != nil {
		t.Fatal(err)
	}
	rec = ts.do(t, "PATCH", url, map[string]any{"content": "x", "attachment_ids": []string{a1.ID, other.ID}}, nil)
	if rec.Code != 400 {
		t.Fatalf("foreign attachment id: %d", rec.Code)
	}

	// Remove a1, keep a2 → 200, a1 blob gone, a2 intact.
	rec = ts.do(t, "PATCH", url, map[string]any{"content": "edited", "attachment_ids": []string{a2.ID}}, nil)
	if rec.Code != 200 {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body)
	}
	var editOut struct {
		AssistantMessageID string `json:"assistant_message_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &editOut); err != nil {
		t.Fatal(err)
	}
	waitForMsg(t, ts.store, editOut.AssistantMessageID, store.StatusComplete)
	if _, err := ts.store.GetAttachment(ctx, a1.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("removed attachment should be gone, got %v", err)
	}
	if _, err := ts.store.GetAttachment(ctx, a2.ID); err != nil {
		t.Fatalf("kept attachment missing: %v", err)
	}

	// Empty content is fine while an attachment remains.
	rec = ts.do(t, "PATCH", url, map[string]any{"content": "", "attachment_ids": []string{a2.ID}}, nil)
	if rec.Code != 200 {
		t.Fatalf("empty content with attachment: %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &editOut); err != nil {
		t.Fatal(err)
	}
	waitForMsg(t, ts.store, editOut.AssistantMessageID, store.StatusComplete)

	// Empty content AND empty keep-list → 400.
	rec = ts.do(t, "PATCH", url, map[string]any{"content": "", "attachment_ids": []string{}}, nil)
	if rec.Code != 400 {
		t.Fatalf("empty content and no attachments: %d", rec.Code)
	}
}

func TestRegenerate(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)
	chatID := ts.createChat(t)

	// No assistant message yet → 404.
	rec := ts.do(t, "POST", "/api/chats/"+chatID+"/regenerate", nil, nil)
	if rec.Code != 404 {
		t.Fatalf("regenerate empty chat: %d", rec.Code)
	}

	rec = ts.do(t, "POST", "/api/chats/"+chatID+"/messages", map[string]any{"content": "hi"}, nil)
	var sendOut struct {
		AssistantMessageID string `json:"assistant_message_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sendOut)
	waitForMsg(t, ts.store, sendOut.AssistantMessageID, store.StatusComplete)

	rec = ts.do(t, "POST", "/api/chats/"+chatID+"/regenerate", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("regenerate: %d %s", rec.Code, rec.Body)
	}
	var regenOut struct {
		AssistantMessageID string `json:"assistant_message_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &regenOut)
	waitForMsg(t, ts.store, regenOut.AssistantMessageID, store.StatusComplete)
	if _, err := ts.store.GetMessage(context.Background(), sendOut.AssistantMessageID); err == nil {
		t.Fatal("old assistant message survived regenerate")
	}
}

func TestAuthMiddlewareGating(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, true)

	// No credentials → 401.
	rec := ts.do(t, "GET", "/api/chats", nil, nil)
	if rec.Code != 401 {
		t.Fatalf("unauthenticated: %d", rec.Code)
	}

	// Login to get a JWT.
	login := ts.do(t, "POST", "/api/auth/login", map[string]any{"username": "admin", "password": "pw"}, nil)
	if login.Code != 200 {
		t.Fatalf("login: %d %s", login.Code, login.Body)
	}
	var loginOut struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &loginOut); err != nil || loginOut.Token == "" {
		t.Fatalf("no token in login response: %v %s", err, login.Body)
	}
	token := loginOut.Token

	// Bearer path.
	rec = ts.do(t, "GET", "/api/auth/me", nil, map[string]string{"Authorization": "Bearer " + token})
	if rec.Code != 200 {
		t.Fatalf("bearer auth: %d", rec.Code)
	}

	// ?token= is accepted on GET routes (EventSource, <img>); non-GET
	// requests must use the Bearer header.
	rec = ts.do(t, "GET", "/api/chats?token="+token, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("token on GET route: %d", rec.Code)
	}
	rec = ts.do(t, "POST", "/api/chats?token="+token, map[string]any{}, nil)
	if rec.Code != 401 {
		t.Fatalf("token on POST route: %d", rec.Code)
	}
	rec = ts.do(t, "GET", "/api/chats/nope/stream?token="+token, nil, nil)
	if rec.Code != 404 { // passes auth, chat missing
		t.Fatalf("token on stream route: %d", rec.Code)
	}

	// Rate limiting: ~5 rapid failures.
	for i := 0; i < 6; i++ {
		login = ts.do(t, "POST", "/api/auth/login", map[string]any{"username": "admin", "password": "bad"}, nil)
	}
	if login.Code != 429 {
		t.Fatalf("rate limit: %d", login.Code)
	}
}

func TestSPAFallbackAndCacheHeaders(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)

	rec := ts.do(t, "GET", "/", nil, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "spa") {
		t.Fatalf("index: %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("index cache-control = %q", cc)
	}
	// Client-side route falls back to index.html.
	rec = ts.do(t, "GET", "/c/some-chat-id", nil, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "spa") {
		t.Fatalf("spa fallback: %d", rec.Code)
	}
	// Hashed assets are immutable.
	rec = ts.do(t, "GET", "/assets/app.js", nil, nil)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %d cc=%q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	// Unknown /api route → 404 JSON, not the SPA.
	rec = ts.do(t, "GET", "/api/definitely-not-a-route", nil, nil)
	if rec.Code != 404 || !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("api 404: %d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

// API JSON responses must opt out of HTTP caching: /api/config mirrors the
// live config store, which can change at any time, and a cached copy leaves
// the client showing a stale model whitelist.
func TestAPIJSONResponsesAreNoStore(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, true)

	rec := ts.do(t, "POST", "/api/auth/login", map[string]any{"username": "admin", "password": "pw"}, nil)
	if rec.Code != 200 {
		t.Fatalf("login: %d", rec.Code)
	}
	var loginOut struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &loginOut); err != nil || loginOut.Token == "" {
		t.Fatalf("no token in login response: %v %s", err, rec.Body)
	}

	rec = ts.do(t, "GET", "/api/config", nil, map[string]string{"Authorization": "Bearer " + loginOut.Token})
	if rec.Code != 200 {
		t.Fatalf("config: %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("config cache-control = %q, want no-store", cc)
	}
}

func TestSSEIdleEvent(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)
	chatID := ts.createChat(t)

	req, _ := http.NewRequest("GET", ts.server.URL+"/api/chats/"+chatID+"/stream", nil)
	resp, err := ts.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	// The first event for a chat with no generation must be "idle".
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), `"type":"idle"`) {
		t.Fatalf("first event not idle: %q", string(buf[:n]))
	}
}

func TestTitleStreamPublishesTitleEvents(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)

	req, _ := http.NewRequest("GET", ts.server.URL+"/api/stream/titles", nil)
	resp, err := ts.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	// By the time Do returns (headers flushed), the subscription is already
	// registered server-side, so this publish cannot be missed.
	ts.titles.Publish("chat-1", "Generated Title")
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	got := string(buf[:n])
	if !strings.Contains(got, `"chat_id":"chat-1"`) || !strings.Contains(got, `"title":"Generated Title"`) {
		t.Fatalf("title event not received: %q", got)
	}
}

func TestUploadValidation(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)
	chatID := ts.createChat(t)

	upload := func(files map[string][]byte) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for name, data := range files {
			fw, err := mw.CreateFormFile("files", name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fw.Write(data); err != nil {
				t.Fatal(err)
			}
		}
		mw.Close()
		req, _ := http.NewRequest("POST", ts.server.URL+"/api/chats/"+chatID+"/attachments", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		resp, err := ts.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		rec := httptest.NewRecorder()
		rec.Code = resp.StatusCode
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(resp.Body)
		rec.Body = body
		return rec
	}

	// Text file → 200.
	rec := upload(map[string][]byte{"notes.md": []byte("# hi\n")})
	if rec.Code != 200 {
		t.Fatalf("text upload: %d %s", rec.Code, rec.Body)
	}
	// Binary junk → 415.
	rec = upload(map[string][]byte{"x.bin": {0x00, 0x01, 0x02}})
	if rec.Code != 415 {
		t.Fatalf("binary upload: %d", rec.Code)
	}
	// Too many files → 400.
	files := map[string][]byte{}
	for i := 0; i < maxUploadFiles+1; i++ {
		files[fmt.Sprintf("f%d.txt", i)] = []byte("x")
	}
	rec = upload(files)
	if rec.Code != 400 {
		t.Fatalf("too many files: %d", rec.Code)
	}
	// Oversize text file → 413 (config cap is 1 MiB in this harness).
	rec = upload(map[string][]byte{"big.txt": bytes.Repeat([]byte("x"), (1<<20)+1)})
	if rec.Code != 413 {
		t.Fatalf("oversize upload: %d", rec.Code)
	}
	// Mixed batch: a valid file plus binary junk. Map iteration order is
	// unspecified, but either way no attachment may survive: when the valid
	// file is processed first the rollback must delete it, when the junk is
	// processed first nothing was stored yet.
	rec = upload(map[string][]byte{"ok.txt": []byte("keep me"), "junk.bin": {0x00, 0x01, 0x02}})
	if rec.Code != 415 {
		t.Fatalf("mixed batch: %d", rec.Code)
	}
	var n int
	if err := ts.db.QueryRow("SELECT COUNT(*) FROM attachments WHERE chat_id = ?", chatID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 { // only the first (valid, single-file) upload may remain
		t.Fatalf("attachments left behind = %d, want 1 (failed batch must roll back)", n)
	}
}

func TestGetAttachmentServing(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)
	chatID := ts.createChat(t)
	meta, err := ts.store.CreateAttachment(context.Background(), chatID, "n.txt", "text", "text/plain", 5, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	rec := ts.do(t, "GET", "/api/attachments/"+meta.ID, nil, nil)
	if rec.Code != 200 || rec.Body.String() != "hello" {
		t.Fatalf("attachment: %d %q", rec.Code, rec.Body.String())
	}
	if cl := rec.Header().Get("Content-Length"); cl != "5" {
		t.Fatalf("content-length = %q", cl)
	}
	// Range request works (http.ServeContent).
	rec = ts.do(t, "GET", "/api/attachments/"+meta.ID, nil, map[string]string{"Range": "bytes=1-3"})
	if rec.Code != 206 || rec.Body.String() != "ell" {
		t.Fatalf("range: %d %q", rec.Code, rec.Body.String())
	}
	// Unknown id → 404.
	rec = ts.do(t, "GET", "/api/attachments/nope", nil, nil)
	if rec.Code != 404 {
		t.Fatalf("unknown attachment: %d", rec.Code)
	}
}
