// Turn timeline for one assistant message. A response is a sequence of
// provider turns: each one thinks, maybe calls tools, and the last one writes
// the answer. MessageItem renders them in that order so a thinking block sits
// with the tool calls it produced.

// How many leading turns of a RELOADED message finished thinking — a thinking
// block shows its checkmark only below this index. Live messages take the
// number from the server's turn_complete events instead.
//
// Every turn but the last necessarily finished (the loop only advances after a
// complete stream). The last one finished too when its tool calls were
// persisted — they are written after the stream ends, before the tools run —
// or when the generation completed.
export function finishedTurns(status, turnCount, toolCalls) {
  if (status === 'complete') return turnCount;
  let lastCall = -1;
  for (const c of toolCalls ?? []) lastCall = Math.max(lastCall, c.turn ?? 0);
  return Math.max(turnCount - 1, lastCall + 1);
}

// How many collapsible boxes the timeline renders (one per thinking block, one
// per tool call) — MessageItem folds them all into a single collapsed
// "Processing…" box as soon as this is more than 1, so two bare boxes are never
// on screen at once. A live reply with nothing yet counts 0 here but still
// shows the empty pill; one box is under the threshold either way, so the pill
// needs no special case.
export function boxCount(turns) {
  let n = 0;
  for (const t of turns) n += (t.text ? 1 : 0) + t.calls.length;
  return n;
}
