// Package store provides the typed repository over the sqlc-generated queries.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"chattoneko/internal/db/query"
)

// ErrNotFound is returned when a lookup has no matching row.
var ErrNotFound = errors.New("not found")

// notFound maps sql.ErrNoRows to ErrNotFound, passing any other error
// through unchanged.
func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// newUUID generates a random UUIDv4.
func newUUID() string { return uuid.NewString() }

// GenParams are per-chat generation options.
type GenParams struct {
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// Chat is a conversation.
type Chat struct {
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	Model     string          `json:"model"`
	Params    GenParams       `json:"params"`
	Tools     map[string]bool `json:"tools"`
	CreatedAt int64           `json:"created_at"`
	UpdatedAt int64           `json:"updated_at"`
}

// ToolCall is a provider tool call recorded on an assistant message.
type ToolCall struct {
	ID             string `json:"id"`
	MessageID      string `json:"message_id"`
	ProviderCallID string `json:"provider_call_id"`
	Name           string `json:"name"`
	Arguments      string `json:"arguments"`
	Position       int64  `json:"position"`
}

// AttachmentMeta is attachment metadata (no blob data).
type AttachmentMeta struct {
	ID        string `json:"id"`
	ChatID    string `json:"chat_id"`
	MessageID string `json:"message_id"`
	Filename  string `json:"filename"`
	Kind      string `json:"kind"`
	Mime      string `json:"mime"`
	Size      int64  `json:"size"`
	CreatedAt int64  `json:"created_at"`
	// HasDescription reports whether a vision-model description is stored
	// for this image attachment (fetchable via the description endpoint).
	HasDescription bool `json:"has_description,omitempty"`
}

// Attachment includes the blob data.
type Attachment struct {
	AttachmentMeta
	Data []byte `json:"-"`
	// Description is the vision-model text description of an image
	// attachment ('' until generated). Cache of what the chat model is
	// shown in place of the image; not part of the API JSON.
	Description string `json:"-"`
}

// Message is a chat message with its tool calls and attachments loaded.
type Message struct {
	Seq        int64  `json:"seq"`
	ID         string `json:"id"`
	ChatID     string `json:"chat_id"`
	Role       string `json:"role"`
	Status     string `json:"status"`
	Content    string `json:"content"`
	Reasoning  string `json:"reasoning"`
	Error      string `json:"error"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
	// Model is the model id that produced this message (assistant messages;
	// '' for messages created before per-message model tracking).
	Model     string `json:"model,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	// Per-turn usage stats (assistant messages; 0 = unknown/not captured).
	PromptTokens     int64            `json:"prompt_tokens,omitempty"`
	CompletionTokens int64            `json:"completion_tokens,omitempty"`
	DurationMs       int64            `json:"duration_ms,omitempty"`
	ToolCalls        []ToolCall       `json:"tool_calls,omitempty"`
	Attachments      []AttachmentMeta `json:"attachments,omitempty"`
}

// Message roles.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message statuses.
const (
	StatusComplete   = "complete"
	StatusGenerating = "generating"
	StatusStopped    = "stopped"
	StatusFailed     = "failed"
)

// Store wraps the sqlc queries.
type Store struct {
	db *sql.DB
	q  *query.Queries
}

// NewStore creates a Store.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db, q: query.New(db)}
}

func nowMillis() int64 { return time.Now().UnixMilli() }

func chatFromRow(c query.Chat) (*Chat, error) {
	var params GenParams
	if c.ParamsJson != "" && c.ParamsJson != "{}" {
		if err := json.Unmarshal([]byte(c.ParamsJson), &params); err != nil {
			return nil, fmt.Errorf("decode params_json for chat %s: %w", c.ID, err)
		}
	}
	tools := map[string]bool{}
	if c.ToolsJson != "" && c.ToolsJson != "{}" {
		if err := json.Unmarshal([]byte(c.ToolsJson), &tools); err != nil {
			return nil, fmt.Errorf("decode tools_json for chat %s: %w", c.ID, err)
		}
	}
	return &Chat{
		ID:        c.ID,
		Title:     c.Title,
		Model:     c.Model,
		Params:    params,
		Tools:     tools,
		CreatedAt: c.CreatedAt,
		UpdatedAt: c.UpdatedAt,
	}, nil
}

func chatsFromRows(rows []query.Chat) ([]*Chat, error) {
	chats := make([]*Chat, 0, len(rows))
	for _, r := range rows {
		c, err := chatFromRow(r)
		if err != nil {
			return nil, err
		}
		chats = append(chats, c)
	}
	return chats, nil
}

func messagesFromRows(rows []query.Message) []*Message {
	msgs := make([]*Message, 0, len(rows))
	for _, r := range rows {
		msgs = append(msgs, messageFromRow(r))
	}
	return msgs
}

func messageFromRow(m query.Message) *Message {
	return &Message{
		Seq:              m.Seq,
		ID:               m.ID,
		ChatID:           m.ChatID,
		Role:             m.Role,
		Status:           m.Status,
		Content:          m.Content,
		Reasoning:        m.Reasoning,
		Error:            m.Error,
		ToolCallID:       m.ToolCallID,
		Name:             m.Name,
		Model:            m.Model,
		CreatedAt:        m.CreatedAt,
		UpdatedAt:        m.UpdatedAt,
		PromptTokens:     m.PromptTokens,
		CompletionTokens: m.CompletionTokens,
		DurationMs:       m.DurationMs,
	}
}

func toolCallFromRow(tc query.ToolCall) ToolCall {
	return ToolCall{
		ID:             tc.ID,
		MessageID:      tc.MessageID,
		ProviderCallID: tc.ProviderCallID,
		Name:           tc.Name,
		Arguments:      tc.Arguments,
		Position:       tc.Position,
	}
}

func attachmentMetaRow(a query.ListAttachmentMetasForChatRow) AttachmentMeta {
	return AttachmentMeta{
		ID:             a.ID,
		ChatID:         a.ChatID,
		MessageID:      a.MessageID,
		Filename:       a.Filename,
		Kind:           a.Kind,
		Mime:           a.Mime,
		Size:           a.Size,
		CreatedAt:      a.CreatedAt,
		HasDescription: a.HasDescription,
	}
}

func attachmentMeta(a query.Attachment) AttachmentMeta {
	return AttachmentMeta{
		ID:             a.ID,
		ChatID:         a.ChatID,
		MessageID:      a.MessageID,
		Filename:       a.Filename,
		Kind:           a.Kind,
		Mime:           a.Mime,
		Size:           a.Size,
		CreatedAt:      a.CreatedAt,
		HasDescription: a.Description != "",
	}
}

// NewChatTitle is the fixed title every new chat starts with.
const NewChatTitle = "New Chat"

// ---- chats ----

// CreateChat inserts a chat with the given settings.
func (s *Store) CreateChat(ctx context.Context, model string, params GenParams, tools map[string]bool) (*Chat, error) {
	// Neither can fail to marshal: a string field and a map[string]bool.
	pj, _ := json.Marshal(params)
	tj, _ := json.Marshal(tools)
	id := newUUID()
	now := nowMillis()
	err := s.q.CreateChat(ctx, query.CreateChatParams{
		ID:         id,
		Title:      NewChatTitle,
		Model:      model,
		ParamsJson: string(pj),
		ToolsJson:  string(tj),
		CreatedAt:  now,
		UpdatedAt:  now,
	})
	if err != nil {
		return nil, fmt.Errorf("create chat: %w", err)
	}
	return s.GetChat(ctx, id)
}

// GetChat fetches a chat.
func (s *Store) GetChat(ctx context.Context, id string) (*Chat, error) {
	row, err := s.q.GetChat(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	return chatFromRow(row)
}

// ListChats returns chats most-recent first. If beforeUpdatedAt > 0, applies
// the compound cursor (updated_at, id).
func (s *Store) ListChats(ctx context.Context, limit int64, beforeUpdatedAt int64, beforeID string) ([]*Chat, error) {
	var rows []query.Chat
	var err error
	if beforeUpdatedAt > 0 {
		rows, err = s.q.ListChatsBefore(ctx, query.ListChatsBeforeParams{
			UpdatedAt:   beforeUpdatedAt,
			UpdatedAt_2: beforeUpdatedAt,
			ID:          beforeID,
			Limit:       limit,
		})
	} else {
		rows, err = s.q.ListChats(ctx, limit)
	}
	if err != nil {
		return nil, err
	}
	return chatsFromRows(rows)
}

// UpdateChatTitle sets the title (manual rename). Also marks the title as
// generated so the background title task never overwrites a user-chosen name.
func (s *Store) UpdateChatTitle(ctx context.Context, id, title string) error {
	return s.q.UpdateChatTitle(ctx, query.UpdateChatTitleParams{
		Title:     title,
		UpdatedAt: nowMillis(),
		ID:        id,
	})
}

// ListChatsNeedingTitle returns ids of chats whose title is not final yet
// (background title task candidates), oldest first.
func (s *Store) ListChatsNeedingTitle(ctx context.Context, limit int64) ([]string, error) {
	return s.q.ListChatsNeedingTitle(ctx, limit)
}

// SetGeneratedTitle writes an auto-generated title, but only if the title is
// still non-final (title_generated = 0). Returns false when a concurrent
// manual rename won the race — the caller must not broadcast in that case.
func (s *Store) SetGeneratedTitle(ctx context.Context, id, title string) (bool, error) {
	n, err := s.q.SetGeneratedTitle(ctx, query.SetGeneratedTitleParams{
		Title:     title,
		UpdatedAt: nowMillis(),
		ID:        id,
	})
	return n > 0, err
}

// MarkTitleGenerated flags the title as final without changing it (the title
// task's give-up / nothing-usable outcome).
func (s *Store) MarkTitleGenerated(ctx context.Context, id string) error {
	return s.q.MarkTitleGenerated(ctx, id)
}

// UpdateChatSettings persists model/params/tools.
func (s *Store) UpdateChatSettings(ctx context.Context, id, model string, params GenParams, tools map[string]bool) error {
	pj, _ := json.Marshal(params)
	tj, _ := json.Marshal(tools)
	return s.q.UpdateChatSettings(ctx, query.UpdateChatSettingsParams{
		Model:      model,
		ParamsJson: string(pj),
		ToolsJson:  string(tj),
		UpdatedAt:  nowMillis(),
		ID:         id,
	})
}

// TouchChat bumps updated_at (recency ordering).
func (s *Store) TouchChat(ctx context.Context, id string) error {
	return s.q.TouchChat(ctx, query.TouchChatParams{UpdatedAt: nowMillis(), ID: id})
}

// DeleteChat removes the chat (cascades messages, tool_calls, attachments).
func (s *Store) DeleteChat(ctx context.Context, id string) error {
	return s.q.DeleteChat(ctx, id)
}

// ChatExists reports whether the chat exists.
func (s *Store) ChatExists(ctx context.Context, id string) (bool, error) {
	n, err := s.q.ChatExists(ctx, id)
	return n > 0, err
}

// ListEmptyChatsOlderThan returns chats with no messages created before cutoff.
func (s *Store) ListEmptyChatsOlderThan(ctx context.Context, cutoffMillis int64) ([]*Chat, error) {
	rows, err := s.q.ListEmptyChatsOlderThan(ctx, cutoffMillis)
	if err != nil {
		return nil, err
	}
	return chatsFromRows(rows)
}

// ---- messages ----

// NewMessageParams holds fields for message creation.
type NewMessageParams struct {
	ChatID     string
	Role       string
	Status     string
	Content    string
	Reasoning  string
	Error      string
	ToolCallID string
	Name       string
	Model      string // assistant messages: the model id used for this generation
}

// CreateMessage inserts a message and returns it (with seq assigned).
func (s *Store) CreateMessage(ctx context.Context, p NewMessageParams) (*Message, error) {
	id := newUUID()
	now := nowMillis()
	err := s.q.CreateMessage(ctx, query.CreateMessageParams{
		ID:         id,
		ChatID:     p.ChatID,
		Role:       p.Role,
		Status:     p.Status,
		Content:    p.Content,
		Reasoning:  p.Reasoning,
		Error:      p.Error,
		ToolCallID: p.ToolCallID,
		Name:       p.Name,
		Model:      p.Model,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
	if err != nil {
		return nil, fmt.Errorf("create message: %w", err)
	}
	return s.GetMessage(ctx, id)
}

// GetMessage fetches a message by id.
func (s *Store) GetMessage(ctx context.Context, id string) (*Message, error) {
	row, err := s.q.GetMessage(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	return messageFromRow(row), nil
}

// ListMessages returns messages of a chat ordered by seq, with tool calls and
// attachment metas attached. Runs exactly 3 queries regardless of message
// count (messages + all tool calls + all attachment metas, grouped in Go).
func (s *Store) ListMessages(ctx context.Context, chatID string) ([]*Message, error) {
	rows, err := s.q.ListMessagesByChat(ctx, chatID)
	if err != nil {
		return nil, err
	}
	msgs := messagesFromRows(rows)
	byID := make(map[string]*Message, len(msgs))
	for _, m := range msgs {
		byID[m.ID] = m
	}
	tcRows, err := s.q.ListToolCallsForChat(ctx, chatID)
	if err != nil {
		return nil, err
	}
	for _, tc := range tcRows {
		if m, ok := byID[tc.MessageID]; ok {
			m.ToolCalls = append(m.ToolCalls, toolCallFromRow(tc))
		}
	}
	attRows, err := s.q.ListAttachmentMetasForChat(ctx, chatID)
	if err != nil {
		return nil, err
	}
	for _, a := range attRows {
		if m, ok := byID[a.MessageID]; ok {
			m.Attachments = append(m.Attachments, attachmentMetaRow(a))
		}
	}
	return msgs, nil
}

// UpdateMessageContent flushes streamed content/reasoning.
func (s *Store) UpdateMessageContent(ctx context.Context, id, content, reasoning string) error {
	return s.q.UpdateMessageContent(ctx, query.UpdateMessageContentParams{
		Content:   content,
		Reasoning: reasoning,
		UpdatedAt: nowMillis(),
		ID:        id,
	})
}

// FinalizeMessage sets terminal status + final content/reasoning/error.
func (s *Store) FinalizeMessage(ctx context.Context, id, status, errText, content, reasoning string) error {
	return s.q.UpdateMessageStatus(ctx, query.UpdateMessageStatusParams{
		Status:    status,
		Error:     errText,
		Content:   content,
		Reasoning: reasoning,
		UpdatedAt: nowMillis(),
		ID:        id,
	})
}

// UpdateMessageUsage records per-turn token usage + wall-clock duration on an
// assistant message.
func (s *Store) UpdateMessageUsage(ctx context.Context, id string, promptTokens, completionTokens, durationMs int64) error {
	return s.q.UpdateMessageUsage(ctx, query.UpdateMessageUsageParams{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		DurationMs:       durationMs,
		UpdatedAt:        nowMillis(),
		ID:               id,
	})
}

// ChatTokenTotals sums prompt/completion tokens across a chat's assistant
// messages (for the top-bar totals).
func (s *Store) ChatTokenTotals(ctx context.Context, chatID string) (prompt int64, completion int64, err error) {
	row, err := s.q.ChatTokenTotals(ctx, chatID)
	if err != nil {
		return 0, 0, err
	}
	return row.PromptTotal, row.CompletionTotal, nil
}

// searchScanWindow bounds how many recent chats a search scans (the query
// is a case-insensitive substring scan, not an index lookup).
const searchScanWindow = 500

// SearchChats returns chats whose title OR any message's content contains
// the query (case-insensitive), most-recent first. Only the most recent
// searchScanWindow chats are scanned — a deliberate bound on the full-table
// substring scan; older chats simply never match.
func (s *Store) SearchChats(ctx context.Context, queryStr string, limit int64) ([]*Chat, error) {
	rows, err := s.q.SearchChats(ctx, query.SearchChatsParams{
		Limit:   searchScanWindow,
		LOWER:   queryStr,
		LOWER_2: queryStr,
		Limit_2: limit,
	})
	if err != nil {
		return nil, err
	}
	return chatsFromRows(rows)
}

// UpdateUserMessageContent edits a user message's content.
func (s *Store) UpdateUserMessageContent(ctx context.Context, id, content string) error {
	return s.q.UpdateUserMessageContent(ctx, query.UpdateUserMessageContentParams{
		Content:   content,
		UpdatedAt: nowMillis(),
		ID:        id,
	})
}

// DeleteMessagesFromSeq removes all messages with seq >= seq in a chat.
func (s *Store) DeleteMessagesFromSeq(ctx context.Context, chatID string, seq int64) error {
	return s.q.DeleteMessagesFromSeq(ctx, query.DeleteMessagesFromSeqParams{ChatID: chatID, Seq: seq})
}

// DeleteMessagesAfterSeq removes all messages with seq > seq in a chat.
func (s *Store) DeleteMessagesAfterSeq(ctx context.Context, chatID string, seq int64) error {
	return s.q.DeleteMessagesAfterSeq(ctx, query.DeleteMessagesAfterSeqParams{ChatID: chatID, Seq: seq})
}

// LastAssistantMessage returns the newest assistant message (ErrNotFound if none).
func (s *Store) LastAssistantMessage(ctx context.Context, chatID string) (*Message, error) {
	row, err := s.q.LastAssistantMessage(ctx, chatID)
	if err != nil {
		return nil, notFound(err)
	}
	return messageFromRow(row), nil
}

// FirstUserMessage returns the oldest user message (ErrNotFound if none) —
// the input the background title task generates a title from.
func (s *Store) FirstUserMessage(ctx context.Context, chatID string) (*Message, error) {
	row, err := s.q.FirstUserMessage(ctx, chatID)
	if err != nil {
		return nil, notFound(err)
	}
	return messageFromRow(row), nil
}

// ListGeneratingMessages returns all messages with status 'generating'
// (crash-recovery sweep).
func (s *Store) ListGeneratingMessages(ctx context.Context) ([]*Message, error) {
	rows, err := s.q.ListGeneratingMessages(ctx)
	if err != nil {
		return nil, err
	}
	return messagesFromRows(rows), nil
}

// ---- tool calls ----

// CreateToolCall persists a provider tool call on an assistant message.
func (s *Store) CreateToolCall(ctx context.Context, messageID, providerCallID, name, arguments string, position int64) (*ToolCall, error) {
	tc := ToolCall{
		ID:             newUUID(),
		MessageID:      messageID,
		ProviderCallID: providerCallID,
		Name:           name,
		Arguments:      arguments,
		Position:       position,
	}
	err := s.q.CreateToolCall(ctx, query.CreateToolCallParams{
		ID:             tc.ID,
		MessageID:      tc.MessageID,
		ProviderCallID: tc.ProviderCallID,
		Name:           tc.Name,
		Arguments:      tc.Arguments,
		Position:       tc.Position,
	})
	if err != nil {
		return nil, fmt.Errorf("create tool call: %w", err)
	}
	return &tc, nil
}

// ListDanglingToolCalls returns tool calls on messageID that have no matching
// role=tool result message (B2 invariant finalize).
func (s *Store) ListDanglingToolCalls(ctx context.Context, messageID string) ([]ToolCall, error) {
	rows, err := s.q.ListDanglingToolCalls(ctx, messageID)
	if err != nil {
		return nil, err
	}
	tcs := make([]ToolCall, 0, len(rows))
	for _, r := range rows {
		tcs = append(tcs, toolCallFromRow(r))
	}
	return tcs, nil
}

// DistinctToolNamesInChat returns tool names referenced anywhere in a chat's
// history (H3: tool defs must include them).
func (s *Store) DistinctToolNamesInChat(ctx context.Context, chatID string) ([]string, error) {
	return s.q.DistinctToolNamesInChat(ctx, chatID)
}

// ---- attachments ----

// CreateAttachment stores an attachment (orphan until linked). The meta is
// constructed from the inputs — no read-back of the (potentially large) blob.
func (s *Store) CreateAttachment(ctx context.Context, chatID, filename, kind, mime string, size int64, data []byte) (*AttachmentMeta, error) {
	return s.createAttachment(ctx, chatID, "", filename, kind, mime, size, data)
}

// CreateLinkedAttachment stores an attachment already bound to a message
// (tool-generated files land on the assistant message that produced them —
// never orphaned, so the orphan sweep can't reap a file mid-generation).
func (s *Store) CreateLinkedAttachment(ctx context.Context, chatID, messageID, filename, kind, mime string, size int64, data []byte) (*AttachmentMeta, error) {
	return s.createAttachment(ctx, chatID, messageID, filename, kind, mime, size, data)
}

func (s *Store) createAttachment(ctx context.Context, chatID, messageID, filename, kind, mime string, size int64, data []byte) (*AttachmentMeta, error) {
	meta := AttachmentMeta{
		ID:        newUUID(),
		ChatID:    chatID,
		MessageID: messageID,
		Filename:  filename,
		Kind:      kind,
		Mime:      mime,
		Size:      size,
		CreatedAt: nowMillis(),
	}
	err := s.q.CreateAttachment(ctx, query.CreateAttachmentParams{
		ID:        meta.ID,
		ChatID:    meta.ChatID,
		MessageID: meta.MessageID,
		Filename:  meta.Filename,
		Kind:      meta.Kind,
		Mime:      meta.Mime,
		Size:      meta.Size,
		Data:      data,
		CreatedAt: meta.CreatedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("create attachment: %w", err)
	}
	return &meta, nil
}

// GetAttachment fetches an attachment including blob data.
func (s *Store) GetAttachment(ctx context.Context, id string) (*Attachment, error) {
	row, err := s.q.GetAttachment(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	return &Attachment{AttachmentMeta: attachmentMeta(row), Data: row.Data, Description: row.Description}, nil
}

// ListAttachmentsByMessage returns attachment metas of a message.
func (s *Store) ListAttachmentsByMessage(ctx context.Context, messageID string) ([]AttachmentMeta, error) {
	rows, err := s.q.ListAttachmentsByMessage(ctx, messageID)
	if err != nil {
		return nil, err
	}
	metas := make([]AttachmentMeta, 0, len(rows))
	for _, a := range rows {
		metas = append(metas, attachmentMeta(a))
	}
	return metas, nil
}

// LinkAttachmentToMessage binds an orphan attachment to a message.
func (s *Store) LinkAttachmentToMessage(ctx context.Context, attachmentID, messageID, chatID string) error {
	return s.q.LinkAttachmentToMessage(ctx, query.LinkAttachmentToMessageParams{
		MessageID: messageID,
		ID:        attachmentID,
		ChatID:    chatID,
	})
}

// SetAttachmentDescription stores the vision-model description of an image
// attachment (” clears it). Write-once in practice: once set, the
// description is reused for every generation that needs it.
func (s *Store) SetAttachmentDescription(ctx context.Context, attachmentID, description string) error {
	return s.q.SetAttachmentDescription(ctx, query.SetAttachmentDescriptionParams{
		Description: description,
		ID:          attachmentID,
	})
}

// DeleteAttachment removes an attachment (blob included) linked to a message.
func (s *Store) DeleteAttachment(ctx context.Context, attachmentID, messageID string) error {
	return s.q.DeleteAttachment(ctx, query.DeleteAttachmentParams{
		ID:        attachmentID,
		MessageID: messageID,
	})
}

// DeleteOrphanAttachments removes unlinked attachments older than cutoff.
func (s *Store) DeleteOrphanAttachments(ctx context.Context, cutoffMillis int64) error {
	return s.q.DeleteOrphanAttachmentsOlderThan(ctx, cutoffMillis)
}

// DeleteDanglingAttachments removes attachments of chatID whose message no
// longer exists (history truncation by regenerate / edit-resend deletes
// messages, and attachments.message_id has no FK to cascade).
func (s *Store) DeleteDanglingAttachments(ctx context.Context, chatID string) error {
	return s.q.DeleteDanglingAttachments(ctx, chatID)
}
