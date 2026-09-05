// Heavy markdown pipeline: incremark-renderer, a marked.js-based incremental
// renderer built for streaming. It splits the source into stable blocks plus a
// mutable tail, re-lexes only what changed, and patches block-level DOM nodes
// in place — so a growing message never re-parses (or repaints) the whole
// document.
//
// Math runs through KaTeX in MathML mode: native <math> rendered with the
// browser's system math fonts, zero webfont downloads — the same output shape
// Temml produced before, and incremark handles $$..$$, $..$, \[..\] and \(..\)
// natively, including mid-stream.
//
// This module is ONLY reachable through the dynamic import in markdown.js, so
// rolldown keeps it in a separate `markdown` chunk fetched after first paint.
// Do not import it statically from a component — that re-merges it into the
// entry chunk and undoes the split.

import {
  IncrementalDomRenderer,
  createDefaultHtmlSanitizer,
  renderMarkdownToString,
} from "incremark-renderer";

const sanitize = createDefaultHtmlSanitizer();

export const options = {
  // breaks: a single newline becomes <br>, matching how models format replies.
  marked: { gfm: true, breaks: true },
  // Models don't emit ::: containers; leaving the parser off keeps a stray
  // ":::" literal instead of swallowing it into an unstyled callout.
  container: false,
  highlight: {
    // Emit the app's own <pre class="codeblock"> rather than incremark's
    // .incremark-code-block wrapper: the theme CSS and MessageItem's per-block
    // copy button both key off that markup, so the look stays identical.
    renderBlock: ({ bodyHtml, codeClassName }) =>
      `<pre class="codeblock"><code class="${codeClassName ?? "hljs"}">` +
      // incremark always appends a trailing newline to the code text; inside a
      // <pre> that renders as an extra blank line. Drop it, keeping any
      // highlight spans that closed after it.
      bodyHtml.replace(/\n(<\/span>)*$/, "$1") +
      "</code></pre>",
  },
  // Links open in a new tab. marked's default renderer emits no target attr
  // and setOptions would clobber incremark's own renderer wholesale, so the
  // attributes go on after sanitizing — the href is already scheme-checked.
  sanitizeHtml: {
    sanitizer: (html) =>
      sanitize(html).replace(/<a /g, '<a target="_blank" rel="noopener noreferrer" '),
  },
};

// Browser-only: patches rendered blocks straight into `el`, leaving finished
// blocks mounted while the stream grows. Clears the plain-text fallback the
// caller may have painted before this chunk landed.
export function createRenderer(el) {
  el.innerHTML = "";
  return new IncrementalDomRenderer(el, options);
}

// DOM-free one-shot render for a complete string (node smoke tests).
export function renderToString(src) {
  return renderMarkdownToString(src ?? "", options);
}
