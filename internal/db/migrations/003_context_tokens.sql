-- prompt_tokens/completion_tokens are the BILLED sum over every provider
-- request of the turn (the tool loop re-sends full history each request), so
-- they can't express how much of the context window the chat occupies.
-- context_tokens is the final request's prompt+completion: the snapshot the
-- next request will resend. Old single-request rows are backfilled below;
-- old multi-request rows stay 0 and the UI falls back to their billed sum.
ALTER TABLE messages ADD COLUMN context_tokens INTEGER NOT NULL DEFAULT 0;

-- Backfill: a turn with no tool calls made exactly one provider request, so
-- its billed sum IS the snapshot. Multi-request turns predate per-request
-- persistence and stay 0; the UI falls back to the billed sum for those.
UPDATE messages SET context_tokens = prompt_tokens + completion_tokens
WHERE role = 'assistant' AND context_tokens = 0
  AND id NOT IN (SELECT message_id FROM tool_calls);
