-- Chattoneko schema.
-- All columns NOT NULL with defaults so no query ever binds NULL.
--
-- Settings live in SQLite: config holds global key/value pairs (structured
-- values are JSON), models holds per-model metadata (modalities, context
-- length, reasoning). internal/config.seedIfEmpty writes the defaults into an
-- empty config table on first start.
--
-- Pre-release: the whole schema is this one file. After the first release,
-- schema changes go into new numbered migration files instead of editing this.

CREATE TABLE chats (
  id TEXT NOT NULL PRIMARY KEY,              -- uuid
  title TEXT NOT NULL DEFAULT '',
  title_generated INTEGER NOT NULL DEFAULT 0,-- 1 = title is final (auto-generated or user-set)
  model TEXT NOT NULL DEFAULT '',
  params_json TEXT NOT NULL DEFAULT '{}',    -- {"reasoning_effort":"..."}
  tools_json TEXT NOT NULL DEFAULT '{}',     -- {"tool_name": true|false} overrides config defaults
  pinned INTEGER NOT NULL DEFAULT 0,         -- server state: reaches every client via chat_updated
  created_at INTEGER NOT NULL,               -- unix millis
  updated_at INTEGER NOT NULL
);
CREATE INDEX idx_chats_updated ON chats(updated_at DESC, id DESC);

CREATE TABLE messages (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,     -- global monotonic order
  id TEXT NOT NULL,                          -- uuid (external id)
  chat_id TEXT NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
  role TEXT NOT NULL,                        -- user | assistant | tool
  status TEXT NOT NULL DEFAULT 'complete',   -- complete | generating | stopped | failed
  content TEXT NOT NULL DEFAULT '',
  -- JSON array with one entry per provider round trip of the tool loop; the
  -- UI renders one thinking block per turn, in place among that turn's tool
  -- calls. '' means no thinking.
  reasoning TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  tool_call_id TEXT NOT NULL DEFAULT '',     -- role=tool: provider call id
  name TEXT NOT NULL DEFAULT '',             -- role=tool: tool name
  model TEXT NOT NULL DEFAULT '',            -- model id that produced this message
  prompt_tokens INTEGER NOT NULL DEFAULT 0,  -- input tokens for the turn
  completion_tokens INTEGER NOT NULL DEFAULT 0, -- output tokens for the turn
  -- Final provider request's prompt+completion for the turn: the snapshot the
  -- next request resends. prompt/completion_tokens are the billed sum over
  -- every request of the turn (the tool loop re-sends full history each time),
  -- so they overstate how much of the context window the chat occupies. 0
  -- means no snapshot; the UI falls back to the billed sum.
  context_tokens INTEGER NOT NULL DEFAULT 0,
  duration_ms INTEGER NOT NULL DEFAULT 0,    -- wall-clock ms for the generation
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_messages_id ON messages(id);
CREATE INDEX idx_messages_chat ON messages(chat_id, seq);
-- ListGeneratingMessages (startup recovery of stuck generations) must not scan
-- messages. Partial index: only transient 'generating' rows, cheap to maintain.
CREATE INDEX idx_messages_generating ON messages(seq) WHERE status = 'generating';

CREATE TABLE tool_calls (
  id TEXT NOT NULL PRIMARY KEY,              -- uuid
  message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  provider_call_id TEXT NOT NULL,
  name TEXT NOT NULL,
  arguments TEXT NOT NULL DEFAULT '',        -- JSON
  position INTEGER NOT NULL,                 -- generation-wide counter: ORDER BY position stays chronological across turns
  turn INTEGER NOT NULL DEFAULT 0            -- provider round trip that produced the call
);
CREATE INDEX idx_tool_calls_message ON tool_calls(message_id);

CREATE TABLE attachments (
  id TEXT NOT NULL PRIMARY KEY,              -- uuid
  chat_id TEXT NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
  filename TEXT NOT NULL,
  kind TEXT NOT NULL,                        -- image | text
  mime TEXT NOT NULL,
  size INTEGER NOT NULL,                     -- original file size
  data BLOB NOT NULL,                        -- image: re-encoded PNG; text: raw UTF-8 bytes
  created_at INTEGER NOT NULL
);
CREATE INDEX idx_attachments_chat ON attachments(chat_id);

-- One file can be shown on several messages, so the links live in a join
-- table. Deleting a message cascades its links away; an attachment left with
-- no links is an orphan, reaped by the startup sweep.
CREATE TABLE message_attachments (
  attachment_id TEXT NOT NULL REFERENCES attachments(id) ON DELETE CASCADE,
  message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  PRIMARY KEY (attachment_id, message_id)
);
-- Listing a message's attachments (every chat load, every tool call that
-- publishes one) walks the link from the message side.
CREATE INDEX idx_message_attachments_message ON message_attachments(message_id);

CREATE TABLE config (
  key TEXT NOT NULL PRIMARY KEY,
  value TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0     -- unix millis
);

CREATE TABLE models (
  model_id TEXT NOT NULL PRIMARY KEY,
  -- JSON array of text|image|video|audio|file. OpenRouter's /models reports
  -- "file" for PDF input, so stored modalities use that word: a fetched model
  -- needs no translation and its chip arrives pre-selected.
  input_modality TEXT NOT NULL DEFAULT '["text"]',
  endpoint TEXT NOT NULL DEFAULT 'chat',     -- chat | transcription | image | speech
  context_length INTEGER NOT NULL DEFAULT 131072,                  -- tokens
  reasoning_efforts TEXT NOT NULL DEFAULT '["low","medium","high"]', -- JSON array of selectable effort levels
  reasoning_default TEXT NOT NULL DEFAULT 'medium',                -- one of reasoning_efforts
  updated_at INTEGER NOT NULL DEFAULT 0                            -- unix millis
);
