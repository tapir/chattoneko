// Node smoke test for frontend logic that doesn't need a browser.
// Run: node test-smoke.mjs

import { Typewriter } from './src/lib/typewriter.js';
import { loadMarkdown, normalizeHeadings, splitHeadingHold } from './src/lib/markdown.js';

// The heavy pipeline is a dynamic import in the browser so it stays off the
// critical path; await it here so the checks below hit the real
// incremark-renderer stack (marked + KaTeX-as-MathML + highlight.js + xss)
// instead of the plain-text fallback. renderToString is DOM-free, so this runs
// in node; the DOM patching contract lives in test-lazy-markdown.mjs.
const impl = await loadMarkdown();
const render = impl.renderToString;

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
const md = render('# Title\n\n```js\nconst x = 1 < 2;\n```\n\n<script>alert(1)</script>\n\n| a | b |\n|---|---|\n| 1 | 2 |');
assert(md.includes('<h1>'), 'heading rendered');
assert(md.includes('hljs'), 'code block highlighted');
assert(md.includes('language-js'), 'code lang captured');
assert(!md.includes('<script>'), 'XSS <script> tag stripped');
assert(md.includes('<table>'), 'GFM table rendered');
console.log('OK markdown+xss');

// --- code block markup: the theme CSS and MessageItem's copy button both key
// off <pre class="codeblock">, so the renderBlock override has to hold. ---
assert(md.includes('<pre class="codeblock"><code class="hljs language-js">'), 'codeblock markup preserved');
const pre = md.slice(md.indexOf('<pre class="codeblock">'), md.indexOf('</pre>') + 6);
assert(!/\n(<\/span>)*<\/code>$/.test(pre), `no trailing blank line inside <pre> (got ${JSON.stringify(pre.slice(-40))})`);
// untagged fence: escaped, no highlighting, same wrapper
assert(render('```\nplain <text>\n```').includes('<pre class="codeblock"><code class="hljs">plain &lt;text&gt;'), 'untagged fence stays plain');
console.log('OK codeblock markup');

// --- link rendering ---
const linkMd = render('[x](https://example.com)');
assert(linkMd.includes('target="_blank"'), 'external target preserved');
assert(linkMd.includes('rel="noopener noreferrer"'), 'noopener preserved');
assert(!render('[j](javascript:alert(1))').includes('javascript:'), 'javascript: href emptied by sanitizer');
assert(!render('hi <img src=x onerror=alert(1)>').includes('onerror'), 'inline event handler stripped');
console.log('OK links');

// --- math (KaTeX in MathML mode -> native <math>, no webfonts) ---
const displayMd = render('$$y = a \\cdot x^k$$');
assert(displayMd.includes('<math'), 'display math rendered ($$..$$)');
assert(displayMd.includes('display="block"'), 'display math is display mode');
// .incremark-math-block is the horizontal-scroll wrapper (see app.css)
assert(displayMd.includes('incremark-math-block'), 'display math wrapped for overflow scrolling');
const inline = render('Energy is $E = mc^2$ obviously');
assert(inline.includes('<math'), 'inline math rendered ($..$)');
assert(!inline.includes('display="block"'), 'inline math is not display mode');
assert(inline.includes('incremark-math-inline'), 'inline math wrapped as inline');
const broken = render('$$\\definitely{\\notreal$$');
assert(broken.length > 0, 'broken LaTeX does not throw');
// bracket delimiters are handled natively — no pre-normalization needed
const bracketDisplay = render('\\[\\frac{a}{b}\\]');
assert(bracketDisplay.includes('<math') && bracketDisplay.includes('display="block"'), '\\[...\\] rendered as display math');
const bracketInline = render('so \\(E=mc^2\\) works');
assert(bracketInline.includes('<math') && !bracketInline.includes('display="block"'), '\\(...\\) rendered as inline math');
const multiLine = render('before\n\\[\ny = a x^k\n\\]\nafter');
assert(multiLine.includes('display="block"'), 'multi-line \\[...\\] block rendered');
const codeSafe = render('```\nconst s = "\\\\[not math\\\\]";\n```');
assert(!codeSafe.includes('<math'), 'bracket delimiters inside code fences untouched');
console.log('OK math');

// --- mid-line ATX headings split onto their own line ---
const midHead = render(normalizeHeadings("I'll search the web for power laws.# Power Law\n\nBody."));
assert(midHead.includes('<h1>Power Law</h1>'), 'mid-line # heading promoted to real heading');
// false positives must survive untouched
assert(!render(normalizeHeadings('Use C# for this.')).includes('<h1'), 'C# not treated as heading');
assert(!render(normalizeHeadings('See example.md#section for details')).includes('<h1'), 'anchor link not a heading');
assert(!render(normalizeHeadings('a hashtag like #topic here')).includes('<h1'), 'hashtag not a heading');
assert(render(normalizeHeadings('# real start heading')).includes('<h1>'), 'real line-start heading still works');
console.log('OK mid-line headings');

// --- streaming holdback: a delta can cut the heading pattern anywhere ---
assert(normalizeHeadings('laws.# Power Law') === 'laws.\n\n# Power Law', 'whole-document normalize');
assert(normalizeHeadings('no heading here') === 'no heading here', 'plain text untouched');
let h = splitHeadingHold('', 'laws.#');
assert(h.emit === 'laws' && h.carry === '.#', `holds the punctuation + # (got ${JSON.stringify(h)})`);
h = splitHeadingHold(h.carry, ' Power Law\n');
assert(h.emit === '.\n\n# Power Law\n' && h.carry === '', `releases once decided (got ${JSON.stringify(h)})`);
// the two halves must reassemble into exactly what the one-shot pass produced
const glued = "I'll search the web for power laws.# Power Law";
let carry = '';
let fed = '';
for (const ch of glued) {
  const step = splitHeadingHold(carry, ch);
  carry = step.carry;
  fed += step.emit;
}
fed += normalizeHeadings(carry);
assert(fed === normalizeHeadings(glued), `char-by-char feed matches one-shot (got ${JSON.stringify(fed)})`);
console.log('OK streaming holdback');

// --- streaming parity: appending deltas must land on the one-shot output ---
const doc = '# Title\n\nSome **bold** and `code` plus $x^2$ math.\n\n```js\nconst a = 1;\n```\n\n- one\n- two\n\nLaws.# Tail Heading\n';
const { StreamMarkdownRenderer } = await import('incremark-renderer');
const stream = new StreamMarkdownRenderer(impl.options);
let carry2 = '';
for (const ch of doc) {
  const step = splitHeadingHold(carry2, ch);
  carry2 = step.carry;
  if (step.emit) stream.append(step.emit);
}
if (carry2) stream.append(normalizeHeadings(carry2));
stream.finalize();
assert(stream.renderToString() === render(normalizeHeadings(doc)), 'char-by-char stream equals one-shot render');
console.log('OK streaming parity');

function assert(cond, msg) {
  if (!cond) {
    console.error('FAIL:', msg);
    process.exit(1);
  }
}
console.log('ALL SMOKE TESTS PASSED');
