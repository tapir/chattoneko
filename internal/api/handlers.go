package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"chattoneko/internal/attach"
	"chattoneko/internal/auth"
	"chattoneko/internal/config"
	"chattoneko/internal/engine"
	"chattoneko/internal/provider"
	"chattoneko/internal/store"
)

const maxUploadFiles = 8

// ---- shared handler plumbing ----

// chatByID fetches a chat, answering 404 when it does not exist and 500 on
// store errors. On failure the response is already written and ok is false.
func (s *Server) chatByID(w http.ResponseWriter, ctx context.Context, id string) (*store.Chat, bool) {
	chat, err := s.store.GetChat(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "chat not found")
		return nil, false
	}
	if err != nil {
		internalError(w, "get chat", err)
		return nil, false
	}
	return chat, true
}

// chatExists answers 404 when the chat is missing and 500 on store errors,
// reporting whether the handler can proceed. (A store failure must not
// surface as "chat not found".)
func (s *Server) chatExists(w http.ResponseWriter, ctx context.Context, id string) bool {
	ok, err := s.store.ChatExists(ctx, id)
	if err != nil {
		internalError(w, "check chat", err)
		return false
	}
	if !ok {
		writeError(w, http.StatusNotFound, "chat not found")
	}
	return ok
}

// startClaimedGeneration consumes the generation claim held by the caller:
// it starts the generation and answers 409/500 on failure. On success it
// bumps the chat timestamp and returns the new assistant message.
func (s *Server) startClaimedGeneration(w http.ResponseWriter, r *http.Request, chatID string) (*store.Message, bool) {
	am, err := s.engine.StartClaimedGeneration(r.Context(), chatID)
	if err != nil {
		if errors.Is(err, engine.ErrGenerationActive) {
			writeError(w, http.StatusConflict, err.Error())
			return nil, false
		}
		internalError(w, "start generation", err)
		return nil, false
	}
	if err := s.store.TouchChat(r.Context(), chatID); err != nil {
		slog.Warn("touch chat", "chat", chatID, "error", err)
	}
	return am, true
}

// reapDanglingAttachments removes attachments whose message was deleted by
// history truncation (attachments.message_id has no FK cascade). Failures
// only log: the truncation itself already succeeded.
func (s *Server) reapDanglingAttachments(ctx context.Context, chatID string) {
	if err := s.store.DeleteDanglingAttachments(ctx, chatID); err != nil {
		slog.Warn("delete dangling attachments", "chat", chatID, "error", err)
	}
}

// attachmentByID fetches an attachment, answering 404 when it does not
// exist and 500 on store errors. On failure the response is already
// written and ok is false.
func (s *Server) attachmentByID(w http.ResponseWriter, ctx context.Context, id string) (*store.Attachment, bool) {
	att, err := s.store.GetAttachment(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "attachment not found")
		return nil, false
	}
	if err != nil {
		internalError(w, "get attachment", err)
		return nil, false
	}
	return att, true
}

// ---- auth ----

func (s *Server) handleMeta(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_enabled":   s.auth.Enabled(),
		"setup_complete": s.cfg.Complete(),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeBodyError(w, err)
		return
	}
	token, err := s.auth.Login(body.Username, body.Password)
	if err != nil {
		if errors.Is(err, auth.ErrRateLimited) {
			writeError(w, http.StatusTooManyRequests, "too many login attempts")
			return
		}
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"username": s.auth.Username(), "token": token})
}

func (s *Server) handleMe(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"username": s.auth.Username()})
}

// ---- config ----

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	writeJSON(w, http.StatusOK, map[string]any{
		"models": map[string]any{
			"whitelist":            cfg.Models.Whitelist,
			"default_chat_model":   cfg.Models.DefaultChatModel,
			"default_vision_model": cfg.Models.DefaultVisionModel,
		},
		"model_info":    modelInfoJSON(s.engine.ModelInfo(r.Context())),
		"tools":         s.tools.Tools(),
		"system_prompt": s.engine.SystemPrompt(),
		"limits": map[string]any{
			"upload_max_file_bytes": cfg.Limits.UploadMaxFileBytes,
			"max_tool_iterations":   cfg.Limits.MaxToolIterations,
			// Exposed so the client can validate attachments it stages
			// locally before uploading them at send time (no upload at
			// attach time).
			"max_upload_files":     maxUploadFiles,
			"max_raw_upload_bytes": attach.MaxRawUploadBytes,
		},
	})
}

// modelInfoJSON maps stored model metadata to the /api/config model_info
// shape the existing clients expect (id + context_window), plus the
// modality fields.
func modelInfoJSON(metas []config.ModelMeta) []map[string]any {
	out := make([]map[string]any, 0, len(metas))
	for _, m := range metas {
		out = append(out, map[string]any{
			"id":                m.ModelID,
			"context_window":    m.ContextLength,
			"reasoning_efforts": m.ReasoningEfforts,
			"reasoning_default": m.ReasoningDefault,
			"input_modality":    m.InputModality,
			"output_modality":   m.OutputModality,
		})
	}
	return out
}

// ---- setup ----
//
// The setup endpoints expose the full server configuration — secrets
// included — for the initial setup flow and later admin edits: the settings
// UI displays and edits the provider API key and MCP header values in
// plain text. Auth is not exposed here at all (it is env-var driven).

// setupConfigJSON is the full config view returned by GET /api/setup.
func setupConfigJSON(c *config.Config, metas []config.ModelMeta) map[string]any {
	servers := make([]map[string]any, 0, len(c.MCPServers))
	for _, s := range c.MCPServers {
		// Go marshals maps with sorted keys, so the JSON stays deterministic
		// (the settings UI compares snapshots to detect unsaved edits).
		headers := s.Headers
		if headers == nil {
			headers = map[string]string{}
		}
		servers = append(servers, map[string]any{
			"name":            s.Name,
			"transport":       s.Transport,
			"command":         s.Command,
			"args":            s.Args,
			"url":             s.URL,
			"headers":         headers,
			"default_enabled": s.DefaultEnabled,
		})
	}
	toolDefaults := c.ToolDefaults
	if toolDefaults == nil {
		toolDefaults = map[string]bool{}
	}
	return map[string]any{
		"system_prompt": c.SystemPrompt,
		"provider": map[string]any{
			"base_url":    c.Provider.BaseURL,
			"api_key":     c.Provider.APIKey,
			"api_key_set": c.Provider.APIKey != "",
		},
		"models": map[string]any{
			"whitelist":            c.Models.Whitelist,
			"default_chat_model":   c.Models.DefaultChatModel,
			"default_task_model":   c.Models.DefaultTaskModel,
			"default_vision_model": c.Models.DefaultVisionModel,
			"metas":                modelMetaJSON(metas),
		},
		"mcp_servers": servers,
		// Global per-tool default toggles. Go marshals maps with sorted keys,
		// so the JSON stays deterministic (the settings UI compares snapshots
		// to detect unsaved edits).
		"tool_defaults": toolDefaults,
		"limits": map[string]any{
			"upload_max_file_bytes":    c.Limits.UploadMaxFileBytes,
			"max_tool_iterations":      c.Limits.MaxToolIterations,
			"mcp_call_timeout_seconds": c.Limits.MCPCallTimeoutSeconds,
		},
		// Auth is deliberately omitted: it is env-var driven (CHATTO_USERNAME /
		// CHATTO_PASSWORD), fixed at startup, and not editable through the API.
	}
}

// modelMetaJSON returns the full per-model metadata (round-trips into
// config.ModelMeta) so the settings UI can render one editable card per
// whitelisted model.
func modelMetaJSON(metas []config.ModelMeta) []map[string]any {
	out := make([]map[string]any, 0, len(metas))
	for _, m := range metas {
		out = append(out, map[string]any{
			"model_id":          m.ModelID,
			"input_modality":    m.InputModality,
			"output_modality":   m.OutputModality,
			"context_length":    m.ContextLength,
			"reasoning_efforts": m.ReasoningEfforts,
			"reasoning_default": m.ReasoningDefault,
		})
	}
	return out
}

func (s *Server) handleGetSetup(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	metas, err := s.cfg.ModelMetas(r.Context(), cfg.Models.Whitelist)
	if err != nil {
		internalError(w, "load model metadata", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"complete": s.cfg.Complete(),
		"config":   setupConfigJSON(cfg, metas),
	})
}

// handlePutSetup applies a partial config update. Only the fields present in
// the body change; absent fields keep their current value. auth.password is
// plaintext here and hashed server-side before anything is stored. On
// success the new (full) config is returned.
func (s *Server) handlePutSetup(w http.ResponseWriter, r *http.Request) {
	var patch config.Patch
	if err := decodeJSON(w, r, &patch); err != nil {
		writeBodyError(w, err)
		return
	}
	if _, err := s.cfg.Update(r.Context(), patch); err != nil {
		var verr *config.ValidationError
		if errors.As(err, &verr) {
			writeError(w, http.StatusBadRequest, verr.Error())
		} else {
			internalError(w, "save settings", err)
		}
		return
	}
	s.handleGetSetup(w, r)
}

// handleSetupModels fetches metadata for one or more models from the
// provider's /models endpoint (when configured and reachable) and stores it
// in the models table. The request may carry unsaved base_url/api_key
// overrides from the settings UI; empty fields fall back to the stored
// provider config. Every requested id gets a row: fields the provider
// doesn't report fall back to the spec defaults, so models on providers
// without OpenRouter-style /models data still get usable metadata.
func (s *Server) handleSetupModels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ModelIDs []string `json:"model_ids"`
		// Optional unsaved provider credentials from the settings UI. When
		// present they take precedence over the stored provider config, so a
		// freshly typed base URL + API key works before the form is saved.
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeBodyError(w, err)
		return
	}
	// Dedup + trim; keep first-seen order.
	seen := map[string]bool{}
	ids := make([]string, 0, len(body.ModelIDs))
	for _, id := range body.ModelIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "model_ids must be a non-empty array of model ids")
		return
	}

	cfg := s.cfg.Get()
	// Resolve the provider to query: explicit overrides win; anything left
	// empty falls back to the stored (database) provider config.
	baseURL := strings.TrimSpace(body.BaseURL)
	if baseURL == "" {
		baseURL = cfg.Provider.BaseURL
	}
	apiKey := body.APIKey
	if apiKey == "" {
		apiKey = cfg.Provider.APIKey
	}
	var (
		fetched  []provider.FetchedModel
		fetchErr error
	)
	if baseURL != "" {
		fetched, fetchErr = provider.FetchModels(r.Context(), baseURL, apiKey)
	}
	byID := map[string]provider.FetchedModel{}
	for _, f := range fetched {
		byID[f.ID] = f
	}

	metas := make([]config.ModelMeta, 0, len(ids))
	sources := make(map[string]string, len(ids))
	for _, id := range ids {
		f, found := byID[id]
		var m config.ModelMeta
		if found {
			m = metaFromFetched(id, f)
			sources[id] = "provider"
		} else {
			m = config.DefaultModelMeta(id)
			sources[id] = "defaults"
		}
		metas = append(metas, m)
	}
	if err := s.cfg.UpsertModelMetas(r.Context(), metas); err != nil {
		internalError(w, "store model metadata", err)
		return
	}

	resp := map[string]any{
		"models": modelInfoJSON(metas),
		"source": sources,
	}
	switch {
	case baseURL == "":
		resp["provider"] = "not configured; defaults stored"
	case fetchErr != nil:
		resp["provider"] = "unreachable (" + fetchErr.Error() + "); defaults stored"
	default:
		resp["provider"] = "ok"
	}
	writeJSON(w, http.StatusOK, resp)
}

// metaFromFetched builds one stored ModelMeta from provider-reported data,
// falling back per field to the spec defaults for everything the provider
// didn't report.
func metaFromFetched(id string, f provider.FetchedModel) config.ModelMeta {
	m := config.DefaultModelMeta(id)
	if len(f.InputModalities) > 0 {
		m.InputModality = append([]string(nil), f.InputModalities...)
	}
	if len(f.OutputModalities) > 0 {
		m.OutputModality = append([]string(nil), f.OutputModalities...)
	}
	if f.ContextLength > 0 {
		m.ContextLength = f.ContextLength
	}
	if len(f.ReasoningEfforts) > 0 {
		m.ReasoningEfforts = append([]string(nil), f.ReasoningEfforts...)
		m.ReasoningDefault = f.ReasoningDefault
	}
	config.SanitizeMeta(&m)
	return m
}

// ---- chats ----

func (s *Server) handleListChats(w http.ResponseWriter, r *http.Request) {
	limit := int64(50)
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	var before int64
	beforeID := r.URL.Query().Get("before_id")
	if v := r.URL.Query().Get("before"); v != "" {
		before, _ = strconv.ParseInt(v, 10, 64)
	}
	// Title + content search (#4): when q is present, return matching chats
	// instead of the recent-conversations page.
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		found, err := s.store.SearchChats(r.Context(), q, 50)
		if err != nil {
			internalError(w, "search chats", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"chats": s.annotateGenerating(found)})
		return
	}
	chats, err := s.store.ListChats(r.Context(), limit, before, beforeID)
	if err != nil {
		internalError(w, "list chats", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": s.annotateGenerating(chats)})
}

// chatListItem is a chat plus its live generation state (used by the sidebar
// breathing title; background chats have no SSE stream to learn it from).
type chatListItem struct {
	*store.Chat
	Generating bool `json:"generating"`
}

func (s *Server) annotateGenerating(chats []*store.Chat) []chatListItem {
	out := make([]chatListItem, 0, len(chats))
	for _, c := range chats {
		out = append(out, chatListItem{Chat: c, Generating: s.engine.HasActiveGeneration(c.ID)})
	}
	return out
}

func (s *Server) handleCreateChat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model  string           `json:"model"`
		Params *store.GenParams `json:"params"`
		// Session-scoped tool overrides chosen before the chat exists; the
		// frontend resets them to config defaults on the next chat switch or
		// page refresh, so they are not user-persistent settings.
		Tools map[string]bool `json:"tools"`
	}
	// An empty body is fine (default chat); malformed JSON is not.
	if err := decodeJSON(w, r, &body); err != nil && !errors.Is(err, io.EOF) {
		writeBodyError(w, err)
		return
	}
	model := body.Model
	if model == "" {
		model = s.cfg.Get().Models.DefaultChatModel
	}
	params := store.GenParams{}
	if body.Params != nil {
		params = *body.Params
	}
	tools := body.Tools
	if tools == nil {
		tools = map[string]bool{}
	}
	chat, err := s.store.CreateChat(r.Context(), model, params, tools)
	if err != nil {
		internalError(w, "create chat", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chat": chat})
}

func (s *Server) handleGetChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	chat, ok := s.chatByID(w, r.Context(), id)
	if !ok {
		return
	}
	msgs, err := s.store.ListMessages(r.Context(), id)
	if err != nil {
		internalError(w, "list messages", err)
		return
	}
	promptTotal, completionTotal, err := s.store.ChatTokenTotals(r.Context(), id)
	if err != nil {
		internalError(w, "chat usage", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chat":     chat,
		"messages": msgs,
		"active":   s.engine.HasActiveGeneration(id),
		// Effective system prompt for this chat (the configured base prompt);
		// shown verbatim in the System sheet.
		"system_prompt": s.engine.SystemPrompt(),
		"usage": map[string]any{
			"prompt_tokens":     promptTotal,
			"completion_tokens": completionTotal,
		},
	})
}

// handleChatLog renders the full conversation log as plain text for debugging
// (opened in a new browser tab). Includes chat settings, the effective system
// prompt, and per-message metadata: model, status, timestamps, usage/duration,
// errors, tool calls (with arguments), tool results, reasoning and attachments.
func (s *Server) handleChatLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	chat, ok := s.chatByID(w, r.Context(), id)
	if !ok {
		return
	}
	msgs, err := s.store.ListMessages(r.Context(), id)
	if err != nil {
		internalError(w, "chat log: list messages", err)
		return
	}

	ts := func(ms int64) string { return time.UnixMilli(ms).UTC().Format(time.RFC3339) }

	// Stream straight to the client: a full history can span hundreds of
	// messages and building it as one string would hold every byte twice.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprintf(w, "Chattoneko Full History (debug log)\n")
	fmt.Fprintf(w, "==============================\n\n")
	fmt.Fprintf(w, "Chat ID:  %s\n", chat.ID)
	fmt.Fprintf(w, "Title:    %s\n", chat.Title)
	fmt.Fprintf(w, "Model:    %s (current chat default; each assistant message records its own)\n", chat.Model)
	fmt.Fprintf(w, "Created:  %s\n", ts(chat.CreatedAt))
	fmt.Fprintf(w, "Updated:  %s\n", ts(chat.UpdatedAt))
	if pj, err := json.Marshal(chat.Params); err == nil && string(pj) != "{}" {
		fmt.Fprintf(w, "Params:   %s\n", pj)
	}
	if len(chat.Tools) > 0 {
		if tj, err := json.Marshal(chat.Tools); err == nil {
			fmt.Fprintf(w, "Tool overrides: %s\n", tj)
		}
	}
	if sp := s.engine.SystemPrompt(); sp != "" {
		fmt.Fprintf(w, "\n--- EFFECTIVE SYSTEM PROMPT ---\n%s\n", sp)
	} else {
		fmt.Fprintf(w, "\n--- EFFECTIVE SYSTEM PROMPT ---\n(empty: no system prompt configured)\n")
	}
	fmt.Fprintf(w, "\n--- MESSAGES (%d) ---\n", len(msgs))
	for _, m := range msgs {
		fmt.Fprint(w, "\n------------------------------------------------\n")
		fmt.Fprintf(w, "seq=%d [%s] role=%s status=%s id=%s\n", m.Seq, ts(m.CreatedAt), m.Role, m.Status, m.ID)
		if m.UpdatedAt != m.CreatedAt {
			fmt.Fprintf(w, "  updated: %s\n", ts(m.UpdatedAt))
		}
		// Per-message model: falls back to the chat default for messages
		// that predate per-message model tracking.
		if m.Role == store.RoleAssistant {
			model := m.Model
			if model == "" {
				model = chat.Model + " (assumed; predates per-message tracking)"
			}
			fmt.Fprintf(w, "  model: %s\n", model)
		}
		if m.Role == store.RoleAssistant && (m.PromptTokens > 0 || m.CompletionTokens > 0 || m.DurationMs > 0) {
			fmt.Fprintf(w, "  usage: input=%d output=%d duration=%dms\n", m.PromptTokens, m.CompletionTokens, m.DurationMs)
		}
		if m.Role == store.RoleTool {
			fmt.Fprintf(w, "  tool result for call_id=%s name=%s\n", m.ToolCallID, m.Name)
		}
		if m.Error != "" {
			fmt.Fprintf(w, "  error: %s\n", m.Error)
		}
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(w, "  TOOL CALL #%d %s (call_id=%s)\n    arguments: %s\n", tc.Position, tc.Name, tc.ProviderCallID, tc.Arguments)
		}
		for _, a := range m.Attachments {
			fmt.Fprintf(w, "  ATTACHMENT id=%s %s (kind=%s, mime=%s, %d bytes)\n", a.ID, a.Filename, a.Kind, a.Mime, a.Size)
			if a.HasDescription {
				if full, err := s.store.GetAttachment(r.Context(), a.ID); err == nil && full.Description != "" {
					fmt.Fprintf(w, "  [image description sent to chat models without image input]\n%s\n", full.Description)
				}
			}
		}
		if m.Reasoning != "" {
			fmt.Fprintf(w, "\n  [reasoning]\n%s\n", m.Reasoning)
		}
		if m.Content != "" {
			fmt.Fprintf(w, "\n%s\n", m.Content)
		}
	}
}

func (s *Server) handlePatchChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	chat, ok := s.chatByID(w, r.Context(), id)
	if !ok {
		return
	}
	var body struct {
		Title  *string          `json:"title"`
		Model  *string          `json:"model"`
		Params *store.GenParams `json:"params"`
		Tools  *map[string]bool `json:"tools"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeBodyError(w, err)
		return
	}

	if body.Title != nil {
		if err := s.store.UpdateChatTitle(r.Context(), id, *body.Title); err != nil {
			internalError(w, "update chat title", err)
			return
		}
		s.engine.BroadcastChatUpdated(id, *body.Title)
	}
	if body.Model != nil || body.Params != nil || body.Tools != nil {
		model := chat.Model
		if body.Model != nil {
			model = *body.Model
		}
		params := chat.Params
		if body.Params != nil {
			params = *body.Params
		}
		tools := chat.Tools
		if body.Tools != nil {
			tools = *body.Tools
		}
		if err := s.store.UpdateChatSettings(r.Context(), id, model, params, tools); err != nil {
			internalError(w, "update chat settings", err)
			return
		}
	}
	updated, err := s.store.GetChat(r.Context(), id)
	if err != nil {
		internalError(w, "patch chat: reload", err)
		return
	}
	if body.Model != nil || body.Params != nil || body.Tools != nil {
		// Broadcast carries the fresh chat so other clients apply settings
		// without a refetch.
		s.engine.BroadcastSettingsUpdated(id, updated)
	}
	writeJSON(w, http.StatusOK, map[string]any{"chat": updated})
}

func (s *Server) handleDeleteChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.engine.CancelForChatDeletion(id)
	if err := s.store.DeleteChat(r.Context(), id); err != nil {
		internalError(w, "delete chat", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- messages ----

func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.chatExists(w, r.Context(), id) {
		return
	}
	// Claim the generation slot BEFORE persisting anything: a concurrent
	// request (cross-tab race) loses the claim up front instead of getting a
	// 409 after leaving an unanswered user message behind.
	if err := s.engine.ClaimGeneration(id); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	release := func() { s.engine.ReleaseClaim(id) }
	// Once the claim is ours, a client that vanishes mid-request (app killed,
	// tab closed on a slow link) must not abort the persistence: the
	// generation itself already runs on the server context, so a canceled
	// r.Context() here would leave a user message with no reply.
	ctx := context.WithoutCancel(r.Context())
	var body struct {
		Content       string   `json:"content"`
		AttachmentIDs []string `json:"attachment_ids"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		release()
		writeBodyError(w, err)
		return
	}
	if len(body.AttachmentIDs) > maxUploadFiles {
		release()
		writeError(w, http.StatusBadRequest, fmt.Sprintf("too many attachments (max %d)", maxUploadFiles))
		return
	}
	if strings.TrimSpace(body.Content) == "" && len(body.AttachmentIDs) == 0 {
		release()
		writeError(w, http.StatusBadRequest, "message content is empty")
		return
	}
	// Every referenced attachment must exist and belong to this chat: the
	// link query silently affects zero rows for foreign ids, which would
	// drop attachments from the message without any error.
	for _, aid := range body.AttachmentIDs {
		att, err := s.store.GetAttachment(ctx, aid)
		if errors.Is(err, store.ErrNotFound) || (err == nil && att.ChatID != id) {
			release()
			writeError(w, http.StatusBadRequest, "attachment not found in this chat")
			return
		}
		if err != nil {
			release()
			internalError(w, "check attachment", err)
			return
		}
	}

	msg, err := s.store.CreateMessage(ctx, store.NewMessageParams{
		ChatID:  id,
		Role:    store.RoleUser,
		Status:  store.StatusComplete,
		Content: body.Content,
	})
	if err != nil {
		release()
		internalError(w, "create user message", err)
		return
	}
	for _, aid := range body.AttachmentIDs {
		if err := s.store.LinkAttachmentToMessage(ctx, aid, msg.ID, id); err != nil {
			release()
			internalError(w, "link attachment", err)
			return
		}
	}
	// Reload with attachments for the broadcast.
	full, err := s.store.GetMessage(ctx, msg.ID)
	if err != nil {
		release()
		internalError(w, "reload user message", err)
		return
	}
	atts, err := s.store.ListAttachmentsByMessage(ctx, msg.ID)
	if err != nil {
		release()
		internalError(w, "reload attachments", err)
		return
	}
	full.Attachments = atts
	s.engine.BroadcastUserMessage(id, full)

	// startClaimedGeneration consumes the claim (releases it on failure).
	am, ok := s.startClaimedGeneration(w, r, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chat_id":              id,
		"user_message":         full,
		"assistant_message_id": am.ID,
	})
}

func (s *Server) handleEditMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	mid := r.PathValue("mid")
	// Claim BEFORE reading: the edit truncates everything after the message
	// and re-generates, so reading first would race a concurrently finishing
	// edit/regenerate (the message may already be deleted — the content
	// update would silently affect zero rows — or its seq shifted,
	// truncating the wrong range).
	if err := s.engine.ClaimGeneration(id); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	release := func() { s.engine.ReleaseClaim(id) }
	// Detached like handleSendMessage: an edit truncates history, and a
	// client disconnect between truncation and start must not strand it.
	ctx := context.WithoutCancel(r.Context())

	msg, err := s.store.GetMessage(ctx, mid)
	if errors.Is(err, store.ErrNotFound) {
		release()
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	if err != nil {
		release()
		internalError(w, "get message", err)
		return
	}
	if msg.ChatID != id {
		release()
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	if msg.Role != store.RoleUser {
		release()
		writeError(w, http.StatusBadRequest, "only user messages can be edited")
		return
	}
	var body struct {
		Content string `json:"content"`
		// AttachmentIDs, when present, is the full keep-list for the
		// message: attachments linked to it but absent from the list are
		// deleted. Nil (omitted) means "leave attachments untouched".
		AttachmentIDs *[]string `json:"attachment_ids"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		release()
		writeBodyError(w, err)
		return
	}
	current, err := s.store.ListAttachmentsByMessage(ctx, mid)
	if err != nil {
		release()
		internalError(w, "list attachments", err)
		return
	}
	// Resolve which attachments (if any) the edit removes, validating the
	// keep-list against the attachments actually linked to this message.
	var removeIDs []string
	keepCount := len(current)
	if body.AttachmentIDs != nil {
		keep := make(map[string]bool, len(*body.AttachmentIDs))
		for _, aid := range *body.AttachmentIDs {
			keep[aid] = true
		}
		keepCount = 0
		for _, a := range current {
			if keep[a.ID] {
				keepCount++
				delete(keep, a.ID)
			} else {
				removeIDs = append(removeIDs, a.ID)
			}
		}
		if len(keep) > 0 {
			release()
			writeError(w, http.StatusBadRequest, "attachment id not linked to this message")
			return
		}
	}
	if strings.TrimSpace(body.Content) == "" && keepCount == 0 {
		release()
		writeError(w, http.StatusBadRequest, "message content is empty")
		return
	}
	if err := s.store.UpdateUserMessageContent(ctx, mid, body.Content); err != nil {
		release()
		internalError(w, "update user message", err)
		return
	}
	for _, aid := range removeIDs {
		if err := s.store.DeleteAttachment(ctx, aid, mid); err != nil {
			release()
			internalError(w, "delete attachment", err)
			return
		}
	}
	// Delete every message after this one, then re-generate from here.
	if err := s.store.DeleteMessagesAfterSeq(ctx, id, msg.Seq); err != nil {
		release()
		internalError(w, "truncate history", err)
		return
	}
	// Attachments of the deleted messages (user uploads and tool-generated
	// files) have no FK cascade — reap them instead of leaking blobs.
	s.reapDanglingAttachments(ctx, id)
	s.engine.BroadcastMessagesReset(id)
	// startClaimedGeneration consumes the claim (releases it on failure).
	am, ok := s.startClaimedGeneration(w, r, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chat_id":              id,
		"assistant_message_id": am.ID,
	})
}

func (s *Server) handleRegenerate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Claim BEFORE locating the last assistant message: reading first would
	// race a concurrently finishing regenerate, whose freshly generated
	// messages sit after the stale seq and would be truncated along with the
	// intended ones.
	if err := s.engine.ClaimGeneration(id); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	release := func() { s.engine.ReleaseClaim(id) }
	ctx := context.WithoutCancel(r.Context()) // see handleSendMessage

	last, err := s.store.LastAssistantMessage(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		release()
		writeError(w, http.StatusNotFound, "no assistant message to regenerate")
		return
	}
	if err != nil {
		release()
		internalError(w, "last assistant message", err)
		return
	}
	// Delete the last assistant message AND everything after it.
	if err := s.store.DeleteMessagesFromSeq(ctx, id, last.Seq); err != nil {
		release()
		internalError(w, "truncate history", err)
		return
	}
	// Reap attachments left dangling by the truncation (no FK cascade).
	s.reapDanglingAttachments(ctx, id)
	s.engine.BroadcastMessagesReset(id)
	// startClaimedGeneration consumes the claim (releases it on failure).
	am, ok := s.startClaimedGeneration(w, r, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chat_id":              id,
		"assistant_message_id": am.ID,
	})
}

func (s *Server) handleStopGeneration(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.engine.StopGeneration(id) {
		writeError(w, http.StatusNotFound, "no active generation")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- SSE ----

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.chatExists(w, r.Context(), id) {
		return
	}
	var after int64
	if v := r.URL.Query().Get("after"); v != "" {
		after, _ = strconv.ParseInt(v, 10, 64)
	}
	ch, unsub := s.engine.Subscribe(id, after)
	defer unsub()
	serveEventStream(w, r, ch)
}

// handleGlobalStream is the all-chats lifecycle stream (generation_started /
// done / chat_updated for every chat, plus a generating_snapshot on connect).
// It lets the sidebar track background generations without attaching a
// per-chat stream to each one.
func (s *Server) handleGlobalStream(w http.ResponseWriter, r *http.Request) {
	ch, unsub := s.engine.SubscribeGlobal()
	defer unsub()
	serveEventStream(w, r, ch)
}

// handleTitleStream is the title task's DEDICATED stream: it carries only
// title events (a chat's auto-generated title became final). Fully
// independent from the engine streams — no shared locks, buffers or event
// types — so generation traffic can never delay or drop a title update.
func (s *Server) handleTitleStream(w http.ResponseWriter, r *http.Request) {
	ch, unsub := s.titles.Subscribe()
	defer unsub()
	serveEventStream(w, r, ch)
}

// serveEventStream writes ch as an SSE stream (event: message, JSON data).
// Generic over the event payload: any JSON-marshalable event type works.
func serveEventStream[T any](w http.ResponseWriter, r *http.Request, ch <-chan T) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return // detached (slow subscriber); client reconnects
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			// A real event, not an SSE comment: EventSource never surfaces
			// comments, so the client could not tell a quiet stream from a
			// half-open socket. Clients reconnect after ~3 missed pings.
			if _, err := fmt.Fprint(w, "event: message\ndata: {\"type\":\"ping\"}\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// ---- attachments ----

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.chatExists(w, r.Context(), id) {
		return
	}
	// The raw multipart body can exceed the per-file stored cap because images
	// are downscaled to a compact PNG during processing; the real cap is
	// enforced on the converted bytes inside attach.Process. This ceiling only
	// guards against pathological upload sizes (per-file raw cap + overhead).
	maxTotal := int64(attach.MaxRawUploadBytes)*maxUploadFiles + 64*1024
	r.Body = http.MaxBytesReader(w, r.Body, maxTotal)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "multipart parse: "+err.Error())
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		writeError(w, http.StatusBadRequest, `no files in form field "files"`)
		return
	}
	if len(files) > maxUploadFiles {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("too many files (max %d)", maxUploadFiles))
		return
	}
	maxFileBytes := s.cfg.Get().Limits.UploadMaxFileBytes
	out := make([]*store.AttachmentMeta, 0, len(files))
	// rollback deletes the attachments stored so far when a later file
	// fails: orphans are only swept at startup, so a rejected file must not
	// leave its predecessors behind in the database. They are unlinked
	// (message_id ''), so DeleteAttachment with an empty message id hits.
	rollback := func() {
		for _, m := range out {
			if err := s.store.DeleteAttachment(r.Context(), m.ID, ""); err != nil {
				slog.Warn("rollback attachment", "id", m.ID, "error", err)
			}
		}
	}
	for _, fh := range files {
		name, err := attach.CleanFilename(fh.Filename)
		if err != nil {
			rollback()
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		f, err := fh.Open()
		if err != nil {
			rollback()
			internalError(w, "open upload", err)
			return
		}
		// Per-file read cap (MaxBytesReader above only bounds the whole body):
		// keep one pathological file from filling RAM before Process's own
		// size check can reject it.
		data, err := io.ReadAll(io.LimitReader(f, attach.MaxRawUploadBytes+1))
		_ = f.Close()
		if err != nil {
			rollback()
			writeError(w, http.StatusBadRequest, "read upload: "+err.Error())
			return
		}
		res, err := attach.Process(name, data, maxFileBytes)
		if err != nil {
			rollback()
			if errors.Is(err, attach.ErrUnsupported) {
				writeError(w, http.StatusUnsupportedMediaType, name+": "+err.Error())
				return
			}
			if errors.Is(err, attach.ErrTooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, name+": "+err.Error())
				return
			}
			writeError(w, http.StatusBadRequest, name+": "+err.Error())
			return
		}
		meta, err := s.store.CreateAttachment(r.Context(), id, name, res.Kind, res.Mime, res.Size, res.Data)
		if err != nil {
			rollback()
			internalError(w, "store attachment", err)
			return
		}
		out = append(out, meta)
	}
	writeJSON(w, http.StatusOK, map[string]any{"attachments": out})
}

func (s *Server) handleGetAttachment(w http.ResponseWriter, r *http.Request) {
	att, ok := s.attachmentByID(w, r.Context(), r.PathValue("id"))
	if !ok {
		return
	}
	// Images are stored as re-encoded PNG. Text attachments are served as
	// text/plain regardless of their detected mime so an HTML/SVG upload can
	// never execute in the app's origin; the download gets the real filename
	// via Content-Disposition.
	ctype := "text/plain; charset=utf-8"
	if att.Kind == attach.KindImage {
		ctype = "image/png"
	} else if cd := mime.FormatMediaType("attachment", map[string]string{"filename": att.Filename}); cd != "" {
		// FormatMediaType emits an RFC 5987 filename* for non-ASCII names and
		// returns "" for invalid ones (possible in legacy rows), which we omit.
		w.Header().Set("Content-Disposition", cd)
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	// ServeContent sets Content-Length and handles Range requests.
	http.ServeContent(w, r, "", time.UnixMilli(att.CreatedAt), bytes.NewReader(att.Data))
}

// handleGetAttachmentDescription serves the cached vision-model description
// of an image attachment (the text the chat model is shown in place of the
// image). 404 when the attachment doesn't exist or was never described.
func (s *Server) handleGetAttachmentDescription(w http.ResponseWriter, r *http.Request) {
	att, ok := s.attachmentByID(w, r.Context(), r.PathValue("id"))
	if !ok {
		return
	}
	if att.Kind != attach.KindImage || att.Description == "" {
		writeError(w, http.StatusNotFound, "no description for this attachment")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, "", time.UnixMilli(att.CreatedAt), bytes.NewReader([]byte(att.Description)))
}
