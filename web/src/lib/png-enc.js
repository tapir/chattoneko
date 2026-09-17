// The indexed-PNG encoder the browser does not have, reached only when the
// server's image_quantization setting is on (it is off by default).
// canvas.toBlob("image/png") always writes truecolor and lossless, which costs
// 2-4 MB for a 1280px photo; UPNG quantizes the canvas's RGBA pixels to a
// 256-colour palette and writes a PNG-8 instead, a few times smaller (measured:
// a 2000x1500 photo 4.3 MB -> 1.4 MB). Pure JS — no wasm, no worker, and no
// encoder to probe for — so unlike an audio conversion it exists in every
// browser.
//
// Deep import of the ESM build: the package declares "type": "module" while
// pointing "main" at a CommonJS file, which node cannot load, and this module is
// covered by a node-run smoke test.
import UPNG from "@upng/upng-js/dist/UPNG.esm.js";

const COLORS = 256;

// encodePNG turns raw RGBA pixels (an ImageData) into indexed PNG bytes. No
// dithering: it costs three times the encode and does not shrink the file.
export function encodePNG(imageData) {
  return UPNG.encode([imageData.data.buffer], imageData.width, imageData.height, COLORS);
}
