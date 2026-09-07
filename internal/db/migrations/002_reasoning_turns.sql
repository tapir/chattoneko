-- Reasoning becomes per-turn: one entry per provider round trip of the tool
-- loop, stored as a JSON array in messages.reasoning. The UI renders one
-- thinking block per turn, in place among that turn's tool calls, instead of
-- merging every turn's thinking into a single block above them all.
-- Existing prose rows become a one-element array; '' stays '' (no thinking).
UPDATE messages SET reasoning = json_array(reasoning) WHERE reasoning <> '';

-- Which turn of the generation produced the call. Needed to interleave tool
-- calls with their turn's thinking block. Old rows keep turn 0, which is what
-- a pre-migration message looks like anyway (one merged block).
ALTER TABLE tool_calls ADD COLUMN turn INTEGER NOT NULL DEFAULT 0;

-- position used to restart at 0 on every iteration, so ORDER BY position
-- scrambled the calls of a multi-turn message on reload. Renumber it as a
-- generation-wide counter (insertion order = chronological order).
UPDATE tool_calls SET position = (
  SELECT COUNT(*) FROM tool_calls t2
  WHERE t2.message_id = tool_calls.message_id
    AND t2.rowid <= tool_calls.rowid
) - 1;
