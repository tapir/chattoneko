// Browser check for the lazily-loaded markdown pipeline.
//
// The node smoke test covers the pipeline itself, but the split introduced a
// reactive contract that only a browser can verify: createRenderer() returns
// null (and the message shows escaped plain text) until the dynamic chunk
// lands, and a component must upgrade when it does. A terminal (non-streaming)
// message is the dangerous case -- nothing else about it changes after mount,
// so if the onMarkdownReady subscription is broken it would stay stuck as
// plain text forever. This also checks the DOM patching half that node cannot
// reach: block wrappers, the pre.codeblock markup the theme CSS targets, and
// native MathML for display math.
//
// Run: npm run dev (in another shell) then `node test-lazy-markdown.mjs`
// Needs: npm i --no-save playwright-core, plus a cached chromium in
// ~/.cache/ms-playwright (same setup as test-mobile-picker.mjs).

import { chromium } from 'playwright-core';
import { writeFile, rm } from 'node:fs/promises';
import { readdirSync, existsSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { homedir } from 'node:os';

// Resolve a cached chromium by hand instead of relying on the exact revision
// playwright-core wants -- the ambient cache and the package version drift.
function findChromium() {
  const base = path.join(homedir(), '.cache', 'ms-playwright');
  if (!existsSync(base)) return undefined;
  for (const dir of readdirSync(base).sort().reverse()) {
    if (!dir.startsWith('chromium-')) continue;
    for (const sub of ['chrome-linux64', 'chrome-linux']) {
      const exe = path.join(base, dir, sub, 'chrome');
      if (existsSync(exe)) return exe;
    }
  }
  return undefined;
}

const ORIGIN = process.env.ORIGIN ?? 'http://localhost:5173';
const HERE = path.dirname(fileURLToPath(import.meta.url));
// Served as a real file from the dev-server root so the ESM import below
// resolves against the origin. page.setContent cannot do this: it builds an
// about:blank document where '/src/...' module specifiers never resolve.
const HARNESS_NAME = '__lazy-markdown-harness.html';

// Harness page: imports the facade the same way MessageItem does and drives it
// through the exact fallback -> ready transition, writing results to the DOM.
const HARNESS = `<!doctype html><html><head><meta charset="utf-8"><title>harness</title></head><body>
  <script type="module">
    import {
      createRenderer, escapeHtml, normalizeHeadings, splitHeadingHold,
      loadMarkdown, isMarkdownReady, onMarkdownReady,
    } from '/src/lib/markdown.js';

    const log = (k, v) => {
      window.__results = window.__results || {};
      window.__results[k] = v;
    };

    const host = document.createElement('div');
    host.className = 'md-body';
    document.body.append(host);

    // 1. BEFORE the chunk has loaded: createRenderer returns null (and
    //    self-triggers the load), so MessageItem paints escaped plain text --
    //    never empty, never a throw.
    log('readyBefore', isMarkdownReady());
    log('rendererBefore', createRenderer(host) === null);
    host.innerHTML = \`<span class="whitespace-pre-wrap">\${escapeHtml('# Title')}</span>\`;
    log('fallbackHtml', host.innerHTML);
    log('fallbackIsPlainText', !host.innerHTML.includes('<h1>') && host.innerHTML.includes('# Title'));

    // 2. Pure helpers must work immediately (they are in the entry chunk).
    log('headingHelperWorks', normalizeHeadings('laws.# Power Law') === 'laws.\\n\\n# Power Law');
    log('holdHelperWorks', splitHeadingHold('', 'laws.#').carry === '.#');

    // 3. Mirror MessageItem: subscribe, flip state, then feed. This is the
    //    terminal-message path the smoke test cannot reach.
    let mdReady = false;
    onMarkdownReady(() => { mdReady = true; });

    await loadMarkdown();
    log('mdReadyFlipped', mdReady);
    log('readyAfter', isMarkdownReady());

    const r = createRenderer(host);
    log('rendererAfter', r !== null);
    log('fallbackCleared', host.querySelector('span.whitespace-pre-wrap') === null);

    r.setMarkdown(normalizeHeadings('# Title\\n\\nSome **bold**.\\n\\n\`\`\`js\\nconst x = 1;\\n\`\`\`\\n\\n$$y = x^2$$'));
    log('finalHtml', host.innerHTML.slice(0, 400));
    log('finalHasHeading', host.querySelector('h1') !== null);
    log('boldRendered', host.querySelector('strong') !== null);
    log('blockWrapper', host.querySelector('[data-incremark-block]') !== null);
    log('codeblockMarkup', host.querySelector('pre.codeblock > code.hljs') !== null);
    log('mathMarkup', host.querySelector('.incremark-math-block math') !== null);
    log('done', true);
  </script>
`;

await writeFile(path.join(HERE, HARNESS_NAME), HARNESS, 'utf8');

const executablePath = findChromium();
const browser = await chromium.launch(executablePath ? { executablePath } : {});
const page = await browser.newPage();

const errors = [];
const failedRequests = [];
page.on('pageerror', (e) => errors.push(`pageerror: ${e.message}`));
// Resource failures carry the URL, which a bare 'console' error does not --
// record them separately so a failure is actually diagnosable.
page.on('requestfailed', (r) => failedRequests.push(`${r.url()} (${r.failure()?.errorText})`));
page.on('response', (r) => {
  if (r.status() >= 400) failedRequests.push(`${r.url()} -> HTTP ${r.status()}`);
});

// favicon.ico is requested by the browser itself and this harness page
// declares none; it says nothing about the markdown pipeline.
const isNoise = (u) => /favicon\.ico/.test(u);

// Track whether the heavy chunk is fetched lazily rather than preloaded.
const requests = [];
page.on('request', (r) => requests.push(r.url()));

let results = {};
try {
  await page.goto(`${ORIGIN}/${HARNESS_NAME}`, { waitUntil: 'load', timeout: 20000 });
  await page.waitForFunction('window.__results && window.__results.done === true', null, { timeout: 20000 });
  results = await page.evaluate('window.__results');
} finally {
  await browser.close();
  await rm(path.join(HERE, HARNESS_NAME), { force: true });
}

let failed = 0;
const assert = (cond, msg, extra) => {
  if (cond) { console.log(`OK   ${msg}`); } else { failed++; console.log(`FAIL ${msg}${extra !== undefined ? ` -> ${JSON.stringify(extra)}` : ''}`); }
};

assert(results.readyBefore === false, 'pipeline not ready before load (chunk really is lazy)');
assert(results.rendererBefore === true, 'createRenderer returns null before the chunk lands');
assert(results.headingHelperWorks === true, 'normalizeHeadings works synchronously from entry chunk');
assert(results.holdHelperWorks === true, 'splitHeadingHold works synchronously from entry chunk');
assert(results.fallbackIsPlainText === true, 'fallback paints readable escaped text, not blank', results.fallbackHtml);
assert(results.fallbackHtml.includes('whitespace-pre-wrap'), 'fallback keeps the plain-text tail markup', results.fallbackHtml);
assert(results.readyAfter === true, 'loadMarkdown() resolves');
assert(results.mdReadyFlipped === true, 'onMarkdownReady subscription fired (terminal messages upgrade)');
assert(results.rendererAfter === true, 'createRenderer returns a renderer once ready');
assert(results.fallbackCleared === true, 'creating the renderer clears the plain-text fallback');
assert(results.finalHasHeading === true, 'setMarkdown produced a real <h1> in the DOM');
assert(results.boldRendered === true, 'inline markup rendered', results.finalHtml);
assert(results.blockWrapper === true, 'blocks patched in as [data-incremark-block] wrappers');
assert(results.codeblockMarkup === true, 'code keeps the pre.codeblock markup the theme CSS targets');
assert(results.mathMarkup === true, 'display math is native MathML inside the scroll wrapper');
assert(errors.length === 0, 'no uncaught page errors', errors);
assert(failedRequests.filter((u) => !isNoise(u)).length === 0, 'no failed module requests', failedRequests);

// Dev serves raw source ESM (/src/lib/markdown.impl.js); a production build
// serves hashed chunks (markdown-BNNU5xJs.js). Accept either shape here --
// the production wiring is verified separately by checking dist/index.html has
// no modulepreload for the markdown chunk.
const mdChunk = requests.filter((u) =>
  /markdown(-\.impl)?(-[\w-]+)?\.js/.test(u),
);
assert(mdChunk.length > 0, 'markdown module fetched on demand', mdChunk);

console.log(failed === 0 ? '\nALL LAZY-MARKDOWN TESTS PASSED' : `\n${failed} FAILURE(S)`);
process.exit(failed === 0 ? 0 : 1);
