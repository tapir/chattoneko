-- Pinning is server state, not a browser preference: it lives on the row and
-- reaches every client through the chat_updated broadcast.
ALTER TABLE chats ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
