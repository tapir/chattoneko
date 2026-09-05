// Dependency-free markdown helpers.
//
// These are pure string functions with ZERO third-party imports, so they stay
// in the entry chunk and are available on first paint. The heavy pipeline
// (marked + Temml + highlight.js + DOMPurify, ~135 kB gzipped) lives in
// markdown.impl.js and is loaded on demand — see markdown.js for the façade.
//
// `stablePrefixLen` in particular is on the streaming hot path: it decides
// which slice of a partially-received message is safe to parse, and it must
// never have to wait for a network round-trip to answer.

export function escapeHtml(s) {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

// Models emit math in both dollar ($x$ / $$x$$) and bracket (\(x\) / \[x\])
// delimiters; the math tokenizer only handles dollars. Normalize bracket
// delimiters to dollar form BEFORE parsing. Also split mid-line ATX headings
// ("…sentence.# Title") onto their own line — models frequently emit a
// heading with no preceding newline, which is invalid markdown and would
// otherwise render as literal text. Fenced code blocks are skipped so literal
// \[ and # sequences in code samples survive untouched.
export function normalizeMathDelimiters(src) {
  // Group consecutive lines into code vs. normal regions at ``` boundaries
  // (fence marker lines stay with their code group so marked still sees the
  // full fenced block), convert only normal regions, re-join 1:1.
  const groups = [];
  let cur = { code: false, lines: [] };
  let inFence = false;
  for (const line of (src ?? "").split("\n")) {
    const fence = /^\s*```/.test(line);
    if (fence && !inFence) {
      if (cur.lines.length) groups.push(cur);
      cur = { code: true, lines: [line] };
      inFence = true;
    } else if (fence) {
      cur.lines.push(line);
      groups.push(cur);
      cur = { code: false, lines: [] };
      inFence = false;
    } else {
      cur.lines.push(line);
    }
  }
  if (cur.lines.length) groups.push(cur);
  return groups
    .map((g) => {
      if (g.code) return g.lines.join("\n");
      let text = g.lines.join("\n");
      // Math tokenizers want the delimiters hugging the content, so strip
      // edge newlines: "$$\n…\n$$" stays a paragraph otherwise.
      text = text
        .replace(
          /\\\[([\s\S]*?)\\\]/g,
          (_m, tex) => `$$${tex.replace(/^\n+|\n+$/g, "")}$$`,
        )
        .replace(
          /\\\(([\s\S]*?)\\\)/g,
          (_m, tex) => `$${tex.replace(/^\n+|\n+$/g, "")}$`,
        );
      // Split a mid-line ATX heading ("…laws.# Power Law") onto its own
      // line so it renders as a heading. Only after sentence punctuation,
      // and the heading must look like one (capital/word after the #'s) —
      // avoids mangling "C#", anchor URLs, "#{ruby}", hashtags, etc.
      text = text.replace(
        /([.!?:;\u2014\u2013])\s*(#{1,6})\s+(?=[A-Z0-9"'\u201C])/g,
        "$1\n\n$2 ",
      );
      return text;
    })
    .join("\n");
}

// Close the markdown syntax a mid-line stream cut left open, so the incomplete
// tail can go through the real pipeline and render bold/code/links the way it
// finally will, instead of showing literal `**` and backticks until the line
// completes. Returns null when the tail must stay literal text: a fence line,
// or an unclosed $$ / \[ display-math block — exactly the cases stablePrefixLen
// pushed into the tail on purpose, so half-parsed LaTeX never reaches Temml.
//
// ponytail: naive run counting. Ignores emphasis inside code spans and skips
// single * and _ (ambiguous with "3 * 4" and snake_case); a wrong closer shows
// one stray marker for under a line and self-corrects at the next promotion.
export function closeOpenInline(src) {
  const s = src ?? "";
  const n = (re) => (s.match(re) ?? []).length;
  if (/^\s*```/.test(s)) return null;
  if (n(/\$\$/g) % 2 === 1) return null;
  if (n(/\\\[/g) > n(/\\\]/g)) return null;
  // Code span: an opener run is closed only by a run of the same length.
  let tick = null;
  for (const run of s.match(/`+/g) ?? []) {
    if (tick === null) tick = run;
    else if (run === tick) tick = null;
  }
  if (tick) return s + tick; // inside code: nothing else can be open
  let out = s;
  if (n(/\*\*/g) % 2 === 1) out += "**";
  if (n(/~~/g) % 2 === 1) out += "~~";
  if (/\]\([^)]*$/.test(s)) out += ")";
  return out;
}

// Streaming split point: the longest prefix of `src` ending at a line
// boundary. While tokens stream in, only this stable prefix goes through the
// full markdown pipeline (marked + hljs + DOMPurify); the incomplete tail
// renders as plain text so letter-by-letter updates never re-parse the
// document. Unclosed constructs (code fences, bold, links) are safe: marked
// renders an unclosed fence to the end of input, and the tail catches up on
// the next promotion.
export function stablePrefixLen(src) {
  let cut = src.lastIndexOf("\n") + 1;
  // Never cut inside an unclosed $$ display-math fence — backtrack to the
  // start of the line that opened it, so raw half-parsed LaTeX never goes
  // through the pipeline mid-stream (the whole block stays in the plain-text
  // tail until the closing fence arrives).
  const prefix = src.slice(0, cut);
  // Find the earliest unclosed display-math opener: `$$` (self-closing pair,
  // so odd count = unclosed) and `\[` (closed by `\]`).
  const openers = [];
  if ((prefix.match(/\$\$/g) ?? []).length % 2 === 1)
    openers.push(prefix.lastIndexOf("$$"));
  const ob = (prefix.match(/\\\[/g) ?? []).length;
  const cb = (prefix.match(/\\\]/g) ?? []).length;
  if (ob > cb) openers.push(prefix.lastIndexOf("\\["));
  if (openers.length > 0) {
    cut = prefix.lastIndexOf("\n", Math.min(...openers) - 1) + 1;
  }
  return cut;
}
