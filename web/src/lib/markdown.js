// Markdown façade.
//
// The pipeline is split across three modules so the heavy third-party stack
// stays OFF the critical path:
//
//   markdown.core.js  pure string helpers, zero deps — bundled into the entry
//   markdown.impl.js  marked + Temml + highlight.js + DOMPurify (~135 kB gzip)
//                     reached ONLY by the dynamic import below -> own chunk
//   markdown.js       this façade; keeps the historical sync call signature
//
// The split only works because markdown.impl.js is imported dynamically. A
// static `import ... from './markdown.impl.js'` anywhere in the reachable
// graph merges it back into the entry chunk — the exact regression this module
// exists to prevent.
//
// While the chunk is in flight `renderMarkdown` returns escaped plain text
// instead of formatted HTML, so a message is readable immediately and upgrades
// in place a moment later. Nothing ever renders blank.

import { escapeHtml } from "./markdown.core.js";

export {
  escapeHtml,
  normalizeMathDelimiters,
  stablePrefixLen,
  closeOpenInline,
} from "./markdown.core.js";

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

// Synchronous render — the signature every call site already expects.
//
// Before the chunk lands it self-triggers the load and returns the source as
// an escaped, newline-preserving span. That markup matches the plain-text tail
// MessageItem already renders mid-stream, so the fallback looks like the
// existing streaming behaviour rather than a flash of unstyled content.
export function renderMarkdown(src) {
  if (impl) return impl.renderMarkdown(src);
  loadMarkdown();
  return `<span class="whitespace-pre-wrap">${escapeHtml(src ?? "")}</span>`;
}
