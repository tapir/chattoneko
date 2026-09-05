// Node smoke test for frontend logic that doesn't need a browser.
// Run: node test-smoke.mjs

import { Typewriter } from './src/lib/typewriter.js';
import { renderMarkdown, stablePrefixLen, closeOpenInline, loadMarkdown } from './src/lib/markdown.js';

// The heavy pipeline is a dynamic import in the browser so it stays off the
// critical path; await it here so renderMarkdown below hits the real marked +
// Temml + highlight.js + DOMPurify stack rather than the plain-text fallback.
await loadMarkdown();

// --- Typewriter (rAF shim) ---
let rafQ = [];
globalThis.requestAnimationFrame = (cb) => (rafQ.push(cb), rafQ.length);
globalThis.cancelAnimationFrame = () => (rafQ = []);

let display = '';
const tw = new Typewriter((d) => (display = d));

tw.push('Hello, ');
tw.push('world!');
// no frames run -> nothing displayed yet, buffer holds everything
assert(display === '', `buffer not drained before frames (got ${JSON.stringify(display)})`);

for (let i = 0; i < 20 && rafQ.length; i++) {
  const cbs = rafQ;
  rafQ = [];
  for (const cb of cbs) cb();
}
assert(display === 'Hello, world!', `typewriter drained fully (got ${JSON.stringify(display)})`);

tw.push(' How');
tw.flush();
assert(display === 'Hello, world! How', `flush sets full display (got ${JSON.stringify(display)})`);
console.log('OK typewriter');

// --- Markdown pipeline ---
const md = renderMarkdown('# Title\n\n```js\nconst x = 1 < 2;\n```\n\n<script>alert(1)</script>\n\n| a | b |\n|---|---|\n| 1 | 2 |');
assert(md.includes('<h1>'), 'heading rendered');
assert(md.includes('hljs'), 'code block highlighted');
assert(md.includes('language-js'), 'code lang captured');
assert(!md.includes('<script>'), 'XSS <script> tag stripped');
// (real DOMPurify in browser mode removes script content entirely)
assert(md.includes('<table>'), 'GFM table rendered');
console.log('OK markdown+xss');

// --- link rendering ---
const linkMd = renderMarkdown('[x](https://example.com)');
assert(linkMd.includes('target="_blank"'), 'external target preserved');
assert(linkMd.includes('noopener'), 'noopener preserved');
console.log('OK links');

// --- math (Temml → MathML) ---
const displayMd = renderMarkdown('$$y = a \\cdot x^k$$');
assert(displayMd.includes('<math'), 'display math rendered ($$..$$)');
assert(displayMd.includes('display="block"'), 'display math is display mode');
const inline = renderMarkdown('Energy is $E = mc^2$ obviously');
assert(inline.includes('<math'), 'inline math rendered ($..$)');
assert(!inline.includes('display="block"'), 'inline math is not display mode');
const broken = renderMarkdown('$$\\definitely{\\notreal$$');
assert(broken.length > 0, 'broken LaTeX does not throw');
// bracket delimiters (\\[...\\] / \\(...\\)) normalize to dollar form
const bracketDisplay = renderMarkdown('\\[\\frac{a}{b}\\]');
assert(bracketDisplay.includes('<math') && bracketDisplay.includes('display="block"'), '\\[...\\] rendered as display math');
const bracketInline = renderMarkdown('so \\(E=mc^2\\) works');
assert(bracketInline.includes('<math') && !bracketInline.includes('display="block"'), '\\(...\\) rendered as inline math');
const multiLine = renderMarkdown('before\n\\[\ny = a x^k\n\\]\nafter');
assert(multiLine.includes('display="block"'), 'multi-line \\[...\\] block rendered');
const codeSafe = renderMarkdown('```\nconst s = "\\\\[not math\\\\]";\n```');
assert(!codeSafe.includes('<math'), 'bracket delimiters inside code fences untouched');
console.log('OK math');

// --- mid-line ATX headings split onto their own line ---
const midHead = renderMarkdown("I'll search the web for power laws.# Power Law\n\nBody.");
assert(midHead.includes('<h1>Power Law</h1>'), 'mid-line # heading promoted to real heading');
// false positives must survive untouched
assert(!renderMarkdown('Use C# for this.').includes('<h1'), 'C# not treated as heading');
assert(!renderMarkdown('See example.md#section for details').includes('<h1'), 'anchor link not a heading');
assert(!renderMarkdown('a hashtag like #topic here').includes('<h1'), 'hashtag not a heading');
assert(renderMarkdown('# real start heading').includes('<h1>'), 'real line-start heading still works');
const codeHead = renderMarkdown('```\nconst s = "text.# NotAHeading";\n```');
assert(!codeHead.includes('<h1'), 'mid-line # inside code fence untouched');
console.log('OK mid-line headings');

// --- streaming split never cuts inside an unclosed $$ fence ---
const mid = 'Intro line\n$$\\frac{a}{b}\nstill math\n';
const cut = stablePrefixLen(mid);
assert(mid.slice(0, cut) === 'Intro line\n', `split backs out of open math block (got ${JSON.stringify(mid.slice(0, cut))})`);
assert(stablePrefixLen('a\nb\n') === 4, 'plain text splits at last newline');
assert(stablePrefixLen('$$x$$\nmore\n') === 11, 'closed math block promotes fully');
const bracketMid = 'Intro\n\\[\ny = x\n';
assert(bracketMid.slice(0, stablePrefixLen(bracketMid)) === 'Intro\n', 'split backs out of open \\[ block');
assert(stablePrefixLen('\\[x\\]\nmore\n') === 11, 'closed \\[...\\] block promotes fully');
console.log('OK streaming math split');

// --- streaming tail: close the syntax a mid-line cut left open ---
assert(closeOpenInline('the **bold') === 'the **bold**', 'unclosed ** gets a closer');
assert(closeOpenInline('a `code') === 'a `code`', 'unclosed backtick gets a closer');
assert(closeOpenInline('~~gone') === '~~gone~~', 'unclosed ~~ gets a closer');
assert(closeOpenInline('see [doc](https://x.y') === 'see [doc](https://x.y)', 'unclosed link gets a )');
assert(closeOpenInline('plain **text** here') === 'plain **text** here', 'balanced markers untouched');
assert(closeOpenInline('snake_case_name and 3 * 4') === 'snake_case_name and 3 * 4', 'single * and _ are never closed');
assert(closeOpenInline('```js') === null, 'fence line stays literal');
assert(closeOpenInline('$$\\frac{a}') === null, 'unclosed $$ display math stays literal');
assert(closeOpenInline('\\[y = x') === null, 'unclosed \\[ block stays literal');
// the point of closing: the tail renders formatted instead of showing markers
assert(renderMarkdown(closeOpenInline('the **bold')).includes('<strong>bold</strong>'), 'closed tail renders bold');
assert(!renderMarkdown(closeOpenInline('a `code')).includes('`'), 'closed tail renders inline code');
console.log('OK streaming tail closing');

function assert(cond, msg) {
  if (!cond) {
    console.error('FAIL:', msg);
    process.exit(1);
  }
}
console.log('ALL SMOKE TESTS PASSED');
