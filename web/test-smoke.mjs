// Node smoke test for frontend logic that doesn't need a browser.
// Run: node test-smoke.mjs

import { Typewriter } from './src/lib/typewriter.js';
import { loadMarkdown, normalizeHeadings, normalizeSource, splitHeadingHold } from './src/lib/markdown.js';

// The heavy pipeline is a dynamic import in the browser so it stays off the
// critical path; await it here so the checks below hit the real
// incremark-renderer stack (marked + KaTeX-as-MathML + highlight.js + xss)
// instead of the plain-text fallback. renderToString is DOM-free, so this runs
// in node; the DOM patching half needs a browser and is not covered here.
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

// --- currency: `$279 and $549` must not be swallowed by KaTeX ---
for (const src of [
  'the Moose Lite at $279 is the answer, the Inspire 2 at $549 is not',
  'Revopoint MINI 2 (~$705 on sale / $829) is the detail pick',
  'Cheapest: **3DMakerpro Moose Lite — ~$279** and the Mole ($325).',
]) {
  const html = render(normalizeSource(src));
  assert(!html.includes('<math'), `price pair not rendered as math: ${src}`);
  assert(html.replaceAll('<strong>', '').includes('$'), `dollar signs survive: ${src}`);
}
// real formulas must still render
for (const f of ['$x^2$', '$2^k$', '$1/\\text{rank}$', '$y = c \\cdot x^k$']) {
  assert(render(normalizeSource(`see ${f} here`)).includes('<math'), `real inline math kept: ${f}`);
}
assert(render(normalizeSource('$$\\sum_{i=1}^n i$$')).includes('<math'), 'block math kept');
assert(render(normalizeSource('inline \\(x+1\\) math')).includes('<math'), 'paren math kept');
console.log('OK currency vs math');

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

// --- AppStream: ONE connection, two halves (routing + seq dedupe) ---
{
  const urls = [];
  class FakeES {
    constructor(url) {
      this.url = url;
      urls.push(url);
      FakeES.live = this;
    }
    close() {}
  }
  globalThis.EventSource = FakeES;
  const { AppStream } = await import('./src/lib/stream.svelte.js');
  const chat = [];
  const glob = [];
  const st = new AppStream('c1', (ev) => chat.push(ev.type), (ev) => glob.push(ev.type));
  assert(urls[0] === '/api/stream?chat=c1&after=-1', `url carries the chat half: ${urls[0]}`);
  const feed = (o) => FakeES.live.onmessage({ data: JSON.stringify(o) });

  feed({ type: 'delta', chat_id: 'c1', seq: 1, epoch: 'e', content: 'a' });
  // The engine fans the open chat's lifecycle events out to the global half
  // too: same seq, so the second copy must be dropped, not handled twice.
  feed({ type: 'generation_started', chat_id: 'c1', seq: 2, epoch: 'e' });
  feed({ type: 'generation_started', chat_id: 'c1', seq: 2, epoch: 'e' });
  // Another chat's lifecycle goes to the sidebar half, and its seq/epoch must
  // NOT poison the open chat's dedupe baseline.
  feed({ type: 'done', chat_id: 'other', seq: 99, epoch: 'zz' });
  feed({ type: 'delta', chat_id: 'c1', seq: 3, epoch: 'e', content: 'b' });
  // Global-only types name a chat but still belong to the sidebar half.
  feed({ type: 'title', chat_id: 'c1', title: 'T' });
  feed({ type: 'generating_snapshot', chat_ids: ['other'] });

  assert(chat.join() === 'delta,generation_started,delta', `chat half got ${chat.join()}`);
  assert(glob.join() === 'done,title,generating_snapshot', `global half got ${glob.join()}`);
  st.close();
  console.log('OK AppStream routing + dedupe');
}

// --- turn timeline: a thinking block is only done when its turn ended ---
{
  const { finishedTurns, boxCount } = await import('./src/lib/turns.js');
  const calls = (...turns) => turns.map((turn) => ({ turn }));
  assert(finishedTurns('complete', 3, calls(0, 1, 2)) === 3, 'a completed generation finished every turn');
  // Stopped mid-stream on turn 2, which never persisted calls: 0-1 are done.
  assert(finishedTurns('stopped', 3, calls(0, 0, 1)) === 2, 'the cut-off turn must stay unfinished');
  // Stopped while turn 2's tools ran: its calls were persisted, so it finished.
  assert(finishedTurns('stopped', 3, calls(0, 1, 2)) === 3, 'a turn whose calls were persisted did finish');
  assert(finishedTurns('failed', 1, []) === 0, 'a failed single turn never finished');
  // Reloaded mid-generation: the turn in flight still spins.
  assert(finishedTurns('generating', 2, calls(0)) === 1, 'the running turn stays unfinished');
  // The "Processing…" fold threshold counts rendered boxes, not turns.
  const box = (text, n) => ({ text, calls: Array.from({ length: n }) });
  assert(boxCount([]) === 0, 'an empty timeline has no boxes');
  assert(boxCount([box('hmm', 0), box('', 0)]) === 1, 'an empty turn is not a box');
  assert(boxCount([box('', 2)]) === 2, 'every tool call is its own box');
  assert(boxCount([box('a', 1), box('', 1)]) === 3, 'boxes add up across turns');
  console.log('OK turn timeline');
}

// --- viewport: which resizes animate (soft keyboard) and which must not ---
// Last because it shims window/document. viewport.js reads the viewport size
// at import time, so the shim has to be in place before the dynamic import.
{
  const vars = {};
  const classes = new Set();
  let onResize = null;
  const win = {
    innerWidth: 400,
    innerHeight: 800,
    visualViewport: { scale: 1, height: 800 },
    addEventListener: (type, fn) => (onResize = fn),
  };
  win.visualViewport.addEventListener = win.addEventListener;
  globalThis.window = win;
  globalThis.document = {
    documentElement: {
      classList: { toggle: (c, on) => (on ? classes.add(c) : classes.delete(c)) },
      style: { setProperty: (k, v) => (vars[k] = v) },
    },
  };
  const { initViewport } = await import('./src/lib/viewport.js');

  // Platforms that resize the LAYOUT viewport: the native WebView, and mobile
  // browsers through interactive-widget=resizes-content.
  const resize = (w, h) => {
    win.innerWidth = w;
    win.innerHeight = h;
    win.visualViewport.height = h;
    onResize();
  };

  initViewport();
  assert(vars['--app-h'] === '800px' && !classes.has('kb-anim'),
    `first measure mirrors the viewport and never animates (got ${vars['--app-h']})`);

  resize(400, 500);
  assert(classes.has('kb-anim') && vars['--app-h'] === '500px', 'keyboard-sized drop animates');
  // One resize fires BOTH listeners (window + visualViewport); the second
  // event sees a zero delta and must not clear the class, or the transition
  // is gone before the first style recalc and the shell snaps.
  onResize();
  assert(classes.has('kb-anim'), 'second event of the burst keeps kb-anim');
  resize(400, 800);
  assert(classes.has('kb-anim') && vars['--app-h'] === '800px', 'keyboard-sized gain animates');
  resize(400, 740);
  assert(!classes.has('kb-anim'), 'browser-chrome-sized delta stays instant');

  // A platform that shrinks only the VISUAL viewport (a WebView that pans,
  // iOS Safari): the shell still has to follow it down.
  win.visualViewport.height = 440;
  onResize();
  assert(classes.has('kb-anim') && vars['--app-h'] === '440px',
    `visual-viewport-only keyboard followed (got ${vars['--app-h']})`);

  // Pinch zoom shrinks the visual viewport too and must be ignored.
  win.visualViewport.scale = 2;
  win.visualViewport.height = 220;
  onResize();
  assert(vars['--app-h'] === '740px', `pinch zoom ignored (got ${vars['--app-h']})`);

  resize(800, 380);
  assert(!classes.has('kb-anim'), 'rotation stays instant');
  console.log('OK viewport keyboard heuristic');
}

// --- attachment text sniff (the client's copy of attach.looksText) ---
{
  const { looksText } = await import('./src/lib/text-sniff.js');
  const file = (bytes) => new File([bytes], 'f');
  const enc = new TextEncoder();
  assert(await looksText(file(enc.encode('# notes\nпривет 😀\ttab'))),
    'utf-8 text accepted regardless of extension');
  assert(await looksText(file(enc.encode('x'))), 'extensionless text accepted');
  assert(!(await looksText(file(new Uint8Array([0x50, 0x4b, 0x03, 0x04, 0x00, 0x01])))),
    'zip (NUL byte) rejected');
  assert(!(await looksText(file(new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])))),
    'png header rejected');
  assert(!(await looksText(file(new Uint8Array([0xff, 0xfe, 0x68, 0x00, 0x69, 0x00])))),
    'utf-16 rejected like the server does');
  assert(await looksText(file(new Uint8Array(0))), 'empty file left to the empty-file check');
  console.log('OK attachment text sniff');
}

// --- long press (the attachment action sheet's trigger) ---
// The whole point is what it does NOT do: message text keeps the browser's
// long press, and a desktop right-click keeps the native menu.
{
  const { longPress } = await import('./src/lib/longpress.js');

  const fakeEl = () => {
    const on = [];
    return {
      addEventListener: (t, fn, o) => on.push({ t, fn, o }),
      removeEventListener: (t, fn) => {
        const i = on.findIndex((l) => l.t === t && l.fn === fn);
        if (i >= 0) on.splice(i, 1);
      },
      // Dispatch a click the way the browser would after a long press.
      click() {
        const e = evt('click');
        for (const l of on.filter((l) => l.t === 'click' && l.o?.capture)) l.fn(e);
        return e;
      },
    };
  };
  const evt = (type, over = {}) => ({
    type,
    defaultPrevented: false,
    stopped: false,
    preventDefault() {
      this.defaultPrevented = true;
    },
    stopPropagation() {
      this.stopped = true;
    },
    ...over,
  });
  const touch = (el, type, x = 0, y = 0) =>
    evt(type, { pointerType: 'touch', isPrimary: true, clientX: x, clientY: y, currentTarget: el });
  const wait = (ms) => new Promise((r) => setTimeout(r, ms));

  // A held touch fires exactly once, and the click that follows is swallowed
  // so the lightbox doesn't open under the sheet.
  let fired = 0;
  const el = fakeEl();
  const h = longPress(() => fired++);
  h.onpointerdown(touch(el, 'pointerdown'));
  await wait(500);
  assert(fired === 1, `held touch fires once (got ${fired})`);
  const click = el.click();
  assert(click.defaultPrevented && click.stopped, 'the click after a long press is swallowed');
  h.onpointerup(touch(el, 'pointerup'));

  // Android fires contextmenu at its own threshold: prevented, and never a
  // second open.
  fired = 0;
  const el2 = fakeEl();
  const h2 = longPress(() => fired++);
  h2.onpointerdown(touch(el2, 'pointerdown'));
  const menu = evt('contextmenu');
  h2.oncontextmenu(menu);
  await wait(500);
  assert(menu.defaultPrevented && fired === 1, 'touch contextmenu opens the sheet once');
  h2.onpointerup(touch(el2, 'pointerup'));

  // A scroll (finger drift, or the browser reclaiming the gesture) cancels.
  fired = 0;
  const h3 = longPress(() => fired++);
  h3.onpointerdown(touch(fakeEl(), 'pointerdown', 10, 10));
  h3.onpointermove(touch(null, 'pointermove', 10, 60));
  await wait(500);
  assert(fired === 0, `drift past the slop cancels (got ${fired})`);

  fired = 0;
  const h4 = longPress(() => fired++);
  const el4 = fakeEl();
  h4.onpointerdown(touch(el4, 'pointerdown'));
  h4.onpointercancel(touch(el4, 'pointercancel'));
  await wait(500);
  assert(fired === 0, 'pointercancel (scroll takeover) cancels');

  // Desktop: a mouse press never fires, and a right-click keeps the native
  // menu — that's the "don't break anything else" half of the contract.
  fired = 0;
  const h5 = longPress(() => fired++);
  h5.onpointerdown(evt('pointerdown', { pointerType: 'mouse', isPrimary: true, currentTarget: el4 }));
  const rc = evt('contextmenu');
  h5.oncontextmenu(rc);
  await wait(500);
  assert(fired === 0 && !rc.defaultPrevented, 'mouse press and right-click stay the browser\'s');
  console.log('OK long press');
}

function assert(cond, msg) {
  if (!cond) {
    console.error('FAIL:', msg);
    process.exit(1);
  }
}
console.log('ALL SMOKE TESTS PASSED');
