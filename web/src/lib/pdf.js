// PDF attachments: which ones preview, and how many pages they have.
//
// Both consumers — the chat's inline page card and the lightbox's document
// pane — render through svelte-pdf, whose entire weight is pdf.js (~1 MB of
// module plus a worker). So nothing here is static but the worker's URL: the
// module and the component are dynamic imports, and a chat with no PDF in it
// never downloads any of it.

import workerUrl from 'pdfjs-dist/build/pdf.worker.min.mjs?url';

let pdfjsReady = null;

// The pdf.js module, with ONE worker shared by every document. svelte-pdf
// points GlobalWorkerOptions.workerSrc at the worker script and pdf.js then
// spawns a worker PER document — and svelte-pdf never destroys its document,
// so a chat with six PDFs would hold six threads for the life of the page.
// A shared workerPort makes every load multiplex one instead (fromPort).
export function pdfjs() {
  return (pdfjsReady ??= import('pdfjs-dist').then((m) => {
    m.GlobalWorkerOptions.workerPort = new Worker(workerUrl, { type: 'module' });
    return m;
  }));
}

// The server keeps a PDF under the binary kind and names it by mime, exactly
// like attach.Type does — the kind alone would also match audio and the
// binaries a tool created.
export const isPdf = (att) => att?.mime === 'application/pdf';

// Page count for the lightbox's "n / N" and its disabled nav buttons:
// svelte-pdf keeps the number to itself. The server answers Range requests
// (ServeContent), so pdf.js reads the count out of the head + trailer and
// destroy() aborts the rest — a couple of round trips, not the whole file.
//
// ponytail: no cMapUrl/standardFontDataUrl, which svelte-pdf gives no way to
// set. pdf.js then substitutes system fonts (fine) and skips predefined CJK
// cMaps — a pre-2010 CJK PDF can render with missing glyphs. Serving
// pdfjs-dist's cmaps/ (1.7 MB) is the fix if that ever shows up.
export async function pdfPages(url) {
  const m = await pdfjs();
  const task = m.getDocument({ url });
  try {
    return (await task.promise).numPages;
  } finally {
    task.destroy();
  }
}
