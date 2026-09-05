// Markdown façade.
//
// The pipeline is split across two modules so the heavy third-party stack
// stays OFF the critical path:
//
//   markdown.js       this file: pure string helpers + lazy loader, zero deps
//   markdown.impl.js  incremark-renderer (marked + KaTeX-as-MathML +
//                     highlight.js + xss) -> reached ONLY by the dynamic
//                     import below, so rolldown gives it its own chunk
//
// The split only works because markdown.impl.js is imported dynamically. A
// static `import ... from './markdown.impl.js'` anywhere in the reachable
// graph merges it back into the entry chunk — the exact regression this module
// exists to prevent.
//
// While the chunk is in flight `createRenderer` returns null and MessageItem
// shows escaped plain text instead, so a message is readable immediately and
// upgrades in place a moment later. Nothing ever renders blank.

let impl = null;
let pending = null;
const listeners = new Set();

// Kick off (or join) the dynamic import. Idempotent: repeated callers share
// one promise and one network request.
export function loadMarkdown() {
  if (impl) return Promise.resolve(impl);
  pending ??= import("./markdown.impl.js").then((m) => {
    impl = m;
    for (const fn of listeners) fn();
    listeners.clear();
    return m;
  });
  return pending;
}

export function isMarkdownReady() {
  return impl !== null;
}

// Subscribe to the pipeline becoming available. Fires immediately if it is
// already loaded. Returns a disposer. Intended for a component $effect that
// writes to $state, which makes any markdown-derived value re-run.
export function onMarkdownReady(fn) {
  if (impl) {
    fn();
    return () => {};
  }
  listeners.add(fn);
  return () => listeners.delete(fn);
}

// Incremental renderer bound to `el`: feed it deltas with .append(), freeze
// the tail with .finalize(), or hand it a whole document with .setMarkdown().
// Returns null until the chunk lands — the caller's $effect re-runs on
// mdReady and picks it up then.
export function createRenderer(el) {
  if (!impl) {
    loadMarkdown();
    return null;
  }
  return impl.createRenderer(el);
}

export function escapeHtml(s) {
  return (s ?? "").replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

// Models frequently glue a heading onto the end of a sentence
// ("…power laws.# Power Law"), which is invalid markdown and would render as
// literal text. Split it onto its own line. Requires sentence punctuation
// before the #'s and a capital/digit/quote after, so "C#", "example.md#anchor"
// and "#hashtag" survive untouched.
const MID_LINE_HEADING = /([.!?:;\u2014\u2013])\s*(#{1,6})\s+(?=[A-Z0-9"'\u201C])/g;

export function normalizeHeadings(src) {
  return (src ?? "").replace(MID_LINE_HEADING, "$1\n\n$2 ");
}

// Streaming feed: normalizeHeadings needs the punctuation BEFORE a "#" to
// decide, and a delta can land anywhere, so hold back a trailing fragment that
// could still become one and release it when the next delta settles it. The
// hold is at most a few characters, and the caller flushes `carry` through
// normalizeHeadings when the stream ends.
//
// ponytail: no code-fence awareness — a literal "…# Word" inside a fenced
// block would get split too. The capital-after-# requirement makes that rare;
// per-delta fence tracking is the upgrade path if it ever shows up.
const HEADING_HOLD = /[.!?:;\u2014\u2013]\s*#{0,6}\s*$/;

export function splitHeadingHold(carry, delta) {
  const buf = (carry ?? "") + (delta ?? "");
  const m = HEADING_HOLD.exec(buf);
  const at = m ? m.index : buf.length;
  return { emit: normalizeHeadings(buf.slice(0, at)), carry: buf.slice(at) };
}
