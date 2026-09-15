-- Pinned chats: a curated section above the sidebar's recents. Pinning is
-- server state, not a browser preference, so it lives on the row and reaches
-- every client through the usual chat_updated broadcast.
ALTER TABLE chats ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
