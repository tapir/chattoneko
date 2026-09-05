// Heavy markdown pipeline: marked (GFM) + math ($$..$$ display, $..$ inline)
// -> highlight.js (manual renderer override) -> DOMPurify. The copy icon is
// attached post-render by MessageItem decoration.
//
// This module is ONLY reachable through the dynamic import in markdown.js, so
// rolldown keeps it in a separate `markdown` chunk that is fetched after first
// paint instead of sitting on the critical path. Do not import it statically
// from a component — that would re-merge it into the entry chunk and undo the
// split (which is exactly what the previous static-import version did).

import { marked } from "marked";
import temml from "temml";
import hljs from "highlight.js/lib/common";
import DOMPurify from "dompurify";
import { escapeHtml, normalizeMathDelimiters } from "./markdown.core.js";

// Math: $...$ inline and $$...$$ display, rendered by Temml to native MathML
// (browser math fonts — zero webfont downloads, unlike KaTeX's ~60 files).
// throwOnError: false renders broken LaTeX as an error annotation in place
// instead of throwing and losing the whole message.
marked.use({
  extensions: [
    {
      name: "inlineMath",
      level: "inline",
      start: (src) => src.indexOf("$"),
      tokenizer(src) {
        // (?!\$): a "$$" opener belongs to the block rule — without this the
        // normalized "$$\n…\n$$" multi-line display block degrades to inline.
        const m = /^\$(?!\$)([^$\n]+?)\$/.exec(src);
        return m ? { type: "inlineMath", raw: m[0], text: m[1] } : undefined;
      },
      renderer: (tok) =>
        temml.renderToString(tok.text, { throwOnError: false }),
    },
    {
      name: "blockMath",
      level: "block",
      tokenizer(src) {
        const m = /^\$\$([\s\S]+?)\$\$/.exec(src);
        return m
          ? { type: "blockMath", raw: m[0], text: m[1].replace(/^\n+|\n+$/g, "") }
          : undefined;
      },
      renderer: (tok) =>
        // .math-scroll wrapper: Firefox ignores overflow-x: auto on <math>
        // (display:math boxes never become scroll containers), so a wide
        // formula would otherwise give the WHOLE chat a horizontal scrollbar
        // on narrow screens. A plain block wrapper scrolls in every engine.
        '<span class="math-scroll">' +
        temml.renderToString(tok.text, {
          displayMode: true,
          throwOnError: false,
        }) +
        "</span>\n",
    },
    {
      // Display math that shares its line with text (e.g. normalized
      // "before\n$$y$$\nafter" stays one paragraph with breaks:true, so the
      // block rule never sees the "$$" at line start). Runs before
      // inlineMath in the tokenizer list; requires a non-empty, non-"$"
      // start so it can't shadow real inline math.
      name: "blockMathInline",
      level: "inline",
      start: (src) => src.indexOf("$$"),
      tokenizer(src) {
        const m = /^\$\$([^$][\s\S]*?)\$\$/.exec(src);
        return m
          ? { type: "blockMathInline", raw: m[0], text: m[1].replace(/^\n+|\n+$/g, "") }
          : undefined;
      },
      renderer: (tok) =>
        // Same .math-scroll wrapper as blockMath (span keeps it valid inside
        // a <p>; CSS makes it a block-level horizontal scroller).
        '<span class="math-scroll">' +
        temml.renderToString(tok.text, {
          displayMode: true,
          throwOnError: false,
        }) +
        "</span>",
    },
  ],
});

// In the browser DOMPurify is a ready instance; in DOM-less environments
// (node smoke tests) it is null. Regex fallback is only exercised there.
const purify = DOMPurify?.sanitize
  ? DOMPurify
  : { sanitize: (raw, _cfg) => raw.replace(/<\/?script[^>]*>/gi, "") };

const renderer = new marked.Renderer();

renderer.code = ({ text, lang }) => {
  const requested = (lang || "").trim().split(/\s+/)[0].toLowerCase();
  const language =
    requested && hljs.getLanguage(requested) ? requested : "plaintext";
  let highlighted;
  try {
    highlighted = hljs.highlight(text, { language }).value;
  } catch {
    highlighted = escapeHtml(text);
  }
  return `<pre class="codeblock"><code class="hljs language-${escapeHtml(language)}">${highlighted}</code></pre>`;
};

renderer.link = ({ href, title, text }) => {
  const safeHref = escapeHtml(href || "");
  const titleAttr = title ? ` title="${escapeHtml(title)}"` : "";
  return `<a href="${safeHref}" target="_blank" rel="noopener noreferrer"${titleAttr}>${text}</a>`;
};

export function renderMarkdown(src) {
  const raw = marked.parse(normalizeMathDelimiters(src ?? ""), {
    renderer,
    gfm: true,
    breaks: true,
    async: false,
  });
  // mathMl profile: Temml's math output IS MathML — without this profile
  // DOMPurify strips the entire rendered formula.
  return purify.sanitize(raw, {
    USE_PROFILES: { html: true, mathMl: true },
    ADD_ATTR: ["target", "rel", "display"],
  });
}
