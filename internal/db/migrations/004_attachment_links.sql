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

INSERT INTO message_attachments (attachment_id, message_id)
SELECT id, message_id FROM attachments WHERE message_id != '';

-- SQLite refuses DROP COLUMN on an indexed column, so the index goes first.
DROP INDEX idx_attachments_message;
ALTER TABLE attachments DROP COLUMN message_id;
