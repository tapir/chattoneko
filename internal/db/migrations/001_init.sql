-- Chattoneko schema.
-- All columns NOT NULL with defaults so no query ever binds NULL.
--
-- Configuration lives in SQLite (config.toml is gone):
--   config  — global settings as key/value pairs; structured values are JSON.
--   models  — per-model metadata (modalities, context length, reasoning).
-- An empty config table is seeded with hardcoded defaults by the Go code on
-- first start (internal/config.seedIfEmpty), not here.

CREATE TABLE chats (
  id TEXT NOT NULL PRIMARY KEY,              -- uuid
  title TEXT NOT NULL DEFAULT '',
  title_generated INTEGER NOT NULL DEFAULT 0,-- 1 = title is final (auto-title ran or user renamed)
  model TEXT NOT NULL DEFAULT '',
  params_json TEXT NOT NULL DEFAULT '{}',    -- {"reasoning_effort":"..."}
  tools_json TEXT NOT NULL DEFAULT '{}',     -- {"tool_name": true|false} overrides config defaults
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
  reasoning TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  tool_call_id TEXT NOT NULL DEFAULT '',     -- role=tool: provider call id
  name TEXT NOT NULL DEFAULT '',             -- role=tool: tool name
  model TEXT NOT NULL DEFAULT '',            -- model id that produced this message
  prompt_tokens INTEGER NOT NULL DEFAULT 0,  -- input tokens for the turn
  completion_tokens INTEGER NOT NULL DEFAULT 0, -- output tokens for the turn
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
  position INTEGER NOT NULL
);
CREATE INDEX idx_tool_calls_message ON tool_calls(message_id);

CREATE TABLE attachments (
  id TEXT NOT NULL PRIMARY KEY,              -- uuid
  chat_id TEXT NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
  message_id TEXT NOT NULL DEFAULT '',       -- linked to user message at send time; '' = orphan
  filename TEXT NOT NULL,
  kind TEXT NOT NULL,                        -- image | text
  mime TEXT NOT NULL,
  size INTEGER NOT NULL,                     -- original file size
  data BLOB NOT NULL,                        -- image: re-encoded PNG; text: raw UTF-8 bytes
  description TEXT NOT NULL DEFAULT '',      -- vision-model description of the image; '' until
                                             -- generated lazily, then cached forever
  created_at INTEGER NOT NULL
);
CREATE INDEX idx_attachments_message ON attachments(message_id);
-- ListAttachmentMetasForChat on every chat load, DeleteDanglingAttachments after
-- every truncation; without it both scan the table holding the largest rows.
CREATE INDEX idx_attachments_chat ON attachments(chat_id);

CREATE TABLE config (
  key TEXT NOT NULL PRIMARY KEY,
  value TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0     -- unix millis
);

CREATE TABLE models (
  model_id TEXT NOT NULL PRIMARY KEY,
  input_modality TEXT NOT NULL DEFAULT '["text"]',                 -- JSON array of text|image|video|audio
  output_modality TEXT NOT NULL DEFAULT '["text"]',                -- JSON array of text|image|video|audio
  context_length INTEGER NOT NULL DEFAULT 131072,                  -- tokens
  reasoning_efforts TEXT NOT NULL DEFAULT '["low","medium","high"]', -- JSON array of selectable effort levels
  reasoning_default TEXT NOT NULL DEFAULT 'medium',                -- one of reasoning_efforts
  updated_at INTEGER NOT NULL DEFAULT 0                            -- unix millis
);
