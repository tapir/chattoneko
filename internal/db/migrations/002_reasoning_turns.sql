-- messages.reasoning is a JSON array with one entry per provider round trip of
-- the tool loop; the UI renders one thinking block per turn, in place among
-- that turn's tool calls. '' means no thinking.
UPDATE messages SET reasoning = json_array(reasoning) WHERE reasoning <> '';

-- Turn of the generation that produced the call, so calls interleave with
-- their turn's thinking block.
ALTER TABLE tool_calls ADD COLUMN turn INTEGER NOT NULL DEFAULT 0;

-- position is a generation-wide counter, so ORDER BY position stays
-- chronological across the turns of one message.
UPDATE tool_calls SET position = (
  SELECT COUNT(*) FROM tool_calls t2
  WHERE t2.message_id = tool_calls.message_id
    AND t2.rowid <= tool_calls.rowid
) - 1;
