-- Attachments stop being owned by one message. The create_file/fetch tools
-- produce a file and hand its id to the model, and attach_file then shows it
-- on a reply — and the same file (an image the user uploaded, a report from
-- earlier in the chat) must be showable again on any later message. A single
-- message_id column could only ever name one owner, so links move to a join
-- table and the column goes away.
--
-- Deleting a message now cascades its links away instead of leaving rows
-- pointing at nothing, which is what DeleteDanglingAttachments used to clean
-- up by hand; an attachment with no links left is an orphan and the existing
-- startup sweep reaps it.

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

-- DROP COLUMN refuses to touch an indexed column, so the old index goes first.
DROP INDEX idx_attachments_message;
ALTER TABLE attachments DROP COLUMN message_id;
