-- context_tokens is the final provider request's prompt+completion for the
-- turn: the snapshot the next request resends. prompt_tokens/completion_tokens
-- are the billed sum over every request of the turn (the tool loop re-sends
-- full history each time), so they overstate how much of the context window
-- the chat occupies. 0 means no snapshot; the UI falls back to the billed sum.
ALTER TABLE messages ADD COLUMN context_tokens INTEGER NOT NULL DEFAULT 0;

-- A turn with no tool calls made exactly one provider request, so its billed
-- sum is the snapshot.
UPDATE messages SET context_tokens = prompt_tokens + completion_tokens
WHERE role = 'assistant' AND context_tokens = 0
  AND id NOT IN (SELECT message_id FROM tool_calls);
