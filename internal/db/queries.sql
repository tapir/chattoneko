-- Chattoneko business queries (sqlc input). Cursor/optional values are
-- expressed as empty strings / sentinel values rather than NULL.

-- ---- chats ----

-- name: CreateChat :exec
INSERT INTO chats (id, title, model, params_json, tools_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetChat :one
SELECT * FROM chats WHERE id = ?;

-- name: ListChats :many
SELECT * FROM chats
ORDER BY updated_at DESC, id DESC
LIMIT ?;

-- name: ListChatsBefore :many
SELECT * FROM chats
WHERE (updated_at < ?)
   OR (updated_at = ? AND id < ?)
ORDER BY updated_at DESC, id DESC
LIMIT ?;

-- Manual rename: also flips title_generated so the background title task
-- never overwrites a user-chosen title (user takes ownership of the title).
-- name: UpdateChatTitle :exec
UPDATE chats SET title = ?, title_generated = 1, updated_at = ? WHERE id = ?;

-- Background title task: chats whose title is not final yet. Manual renames
-- flip the flag, and the task itself sets it when done.
-- name: ListChatsNeedingTitle :many
SELECT id FROM chats WHERE title_generated = 0 ORDER BY created_at ASC LIMIT ?;

-- Conditional title write for the background task: the title_generated = 0
-- guard makes a concurrent manual rename win (rows affected = 0 => skip the
-- SSE broadcast; the user's title stays).
-- name: SetGeneratedTitle :execrows
UPDATE chats SET title = ?, title_generated = 1, updated_at = ? WHERE id = ? AND title_generated = 0;

-- Bookkeeping-only: title stays as-is ("New Chat"), just stop retrying.
-- Does NOT touch updated_at (nothing user-visible changed).
-- name: MarkTitleGenerated :exec
UPDATE chats SET title_generated = 1 WHERE id = ?;

-- name: UpdateChatSettings :exec
UPDATE chats SET model = ?, params_json = ?, tools_json = ?, updated_at = ? WHERE id = ?;

-- name: TouchChat :exec
UPDATE chats SET updated_at = ? WHERE id = ?;

-- name: DeleteChat :exec
DELETE FROM chats WHERE id = ?;

-- name: ChatExists :one
SELECT COUNT(*) FROM chats WHERE id = ?;

-- Title OR message-content search (#4): matches chats whose title contains
-- the query, and chats with any message whose content contains it (so
-- untitled chats and content-only matches are findable). Scans only the
-- most recent chats (scan window) so the full-table instr() scan stays
-- bounded; FTS5 would be the proper fix if search grows in importance.
-- name: SearchChats :many
SELECT c.* FROM chats c
WHERE c.id IN (SELECT id FROM chats ORDER BY updated_at DESC, id DESC LIMIT ?)
  AND (
    instr(lower(c.title), lower(?)) > 0
    OR EXISTS (
      SELECT 1 FROM messages m
      WHERE m.chat_id = c.id
        AND instr(lower(m.content), lower(?)) > 0
    )
  )
ORDER BY c.updated_at DESC, c.id DESC
LIMIT ?;

-- name: ListEmptyChatsOlderThan :many
SELECT c.* FROM chats c
WHERE c.created_at < ?
  AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.chat_id = c.id);

-- ---- messages ----

-- name: CreateMessage :exec
INSERT INTO messages (id, chat_id, role, status, content, reasoning, error, tool_call_id, name, model, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetMessage :one
SELECT * FROM messages WHERE id = ?;

-- name: ListMessagesByChat :many
SELECT * FROM messages WHERE chat_id = ? ORDER BY seq ASC;

-- name: UpdateMessageContent :exec
UPDATE messages SET content = ?, reasoning = ?, updated_at = ? WHERE id = ?;

-- name: UpdateMessageStatus :exec
UPDATE messages SET status = ?, error = ?, content = ?, reasoning = ?, updated_at = ? WHERE id = ?;

-- name: UpdateMessageUsage :exec
UPDATE messages SET prompt_tokens = ?, completion_tokens = ?, duration_ms = ?, updated_at = ? WHERE id = ?;

-- name: ChatTokenTotals :one
SELECT
  -- CAST gives sqlc a declared INTEGER type for the aggregate expressions.
  CAST(COALESCE(SUM(prompt_tokens), 0) AS INTEGER) AS prompt_total,
  CAST(COALESCE(SUM(completion_tokens), 0) AS INTEGER) AS completion_total
FROM messages
WHERE chat_id = ? AND role = 'assistant';

-- name: UpdateUserMessageContent :exec
UPDATE messages SET content = ?, updated_at = ? WHERE id = ? AND role = 'user';

-- name: DeleteMessagesFromSeq :exec
DELETE FROM messages WHERE chat_id = ? AND seq >= ?;

-- name: DeleteMessagesAfterSeq :exec
DELETE FROM messages WHERE chat_id = ? AND seq > ?;

-- name: LastAssistantMessage :one
SELECT * FROM messages
WHERE chat_id = ? AND role = 'assistant'
ORDER BY seq DESC
LIMIT 1;

-- First user message of a chat (the title task's input).
-- name: FirstUserMessage :one
SELECT * FROM messages
WHERE chat_id = ? AND role = 'user'
ORDER BY seq ASC
LIMIT 1;

-- name: ListGeneratingMessages :many
SELECT * FROM messages WHERE status = 'generating';

-- ---- tool_calls ----

-- name: CreateToolCall :exec
INSERT INTO tool_calls (id, message_id, provider_call_id, name, arguments, position)
VALUES (?, ?, ?, ?, ?, ?);

-- name: ListToolCallsByMessage :many
SELECT * FROM tool_calls WHERE message_id = ? ORDER BY position ASC;

-- name: ListToolCallsForChat :many
SELECT tc.* FROM tool_calls tc
JOIN messages m ON m.id = tc.message_id
WHERE m.chat_id = ?
ORDER BY tc.position ASC;

-- name: DistinctToolNamesInChat :many
SELECT DISTINCT tc.name
FROM tool_calls tc
JOIN messages m ON m.id = tc.message_id
WHERE m.chat_id = ?;

-- name: ListDanglingToolCalls :many
SELECT tc.*
FROM tool_calls tc
JOIN messages m ON m.id = tc.message_id
WHERE tc.message_id = ?
  AND NOT EXISTS (
    SELECT 1 FROM messages r
    WHERE r.chat_id = m.chat_id
      AND r.role = 'tool'
      AND r.tool_call_id = tc.provider_call_id
  );

-- ---- attachments ----

-- name: CreateAttachment :exec
INSERT INTO attachments (id, chat_id, message_id, filename, kind, mime, size, data, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetAttachment :one
SELECT * FROM attachments WHERE id = ?;

-- name: ListAttachmentsByMessage :many
SELECT * FROM attachments WHERE message_id = ? ORDER BY created_at ASC;

-- Attachment metas for a whole chat (no blob data) - used to attach metas
-- to messages in one query instead of one query per message.
-- name: ListAttachmentMetasForChat :many
SELECT id, chat_id, message_id, filename, kind, mime, size, created_at,
       CAST((description != '') AS BOOLEAN) AS has_description
FROM attachments WHERE chat_id = ? ORDER BY created_at ASC;

-- name: SetAttachmentDescription :exec
UPDATE attachments SET description = ? WHERE id = ?;

-- name: LinkAttachmentToMessage :exec
UPDATE attachments SET message_id = ? WHERE id = ? AND chat_id = ?;

-- name: DeleteOrphanAttachmentsOlderThan :exec
DELETE FROM attachments WHERE message_id = '' AND created_at < ?;

-- Attachments whose message was truncated away (regenerate / edit-resend).
-- attachments.message_id has no FK (uploads legitimately start unlinked), so
-- deleting messages leaves linked rows dangling; this cleans them up.
-- name: DeleteDanglingAttachments :exec
DELETE FROM attachments
WHERE attachments.chat_id = ? AND attachments.message_id != ''
  AND attachments.message_id NOT IN
    (SELECT m.id FROM messages m WHERE m.chat_id = attachments.chat_id);

-- name: DeleteAttachment :exec
DELETE FROM attachments WHERE id = ? AND message_id = ?;

