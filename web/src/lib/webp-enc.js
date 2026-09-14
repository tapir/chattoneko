// Safari's WebP encoder, which the browser does not have: canvas.toBlob asks
// for "image/webp" and silently hands back a PNG (see lib/media.js), and the
// server stores WebP only. So this is libwebp compiled to WASM, @jsquash/webp's
// non-SIMD encoder build — deep-imported straight from its codec directory
// rather than through the package's own encode(), which would also pull in
// wasm-feature-detect, the SIMD build and the decoder. 281 KB of wasm plus ~39 KB
// of glue that only a Safari image upload ever fetches: media.js reaches this
// module through a dynamic import().
import moduleFactory from "@jsquash/webp/codec/enc/webp_enc.js";
import { defaultOptions } from "@jsquash/webp/meta.js";

// One instance for the whole session: instantiating is most of the cost of an
// encode, and a chat can take several pictures.
let emscriptenModule;

// encodeWebP turns raw RGBA pixels into WebP bytes. quality is on canvas's 0-1
// scale, the same IMAGE_QUALITY media.js passes toBlob; libwebp wants 0-100.
// Every other option stays at the library default (method 4, one pass) — the
// embind struct wants a value for each field, and matching toBlob's output is
// the whole job.
export async function encodeWebP(imageData, quality) {
  const module = await (emscriptenModule ??= moduleFactory({
    // Never invoke a wasm function before the module is ready.
    noInitialRun: true,
  }));
  const out = module.encode(imageData.data, imageData.width, imageData.height, {
    ...defaultOptions,
    quality: Math.round(quality * 100),
  });
  if (!out) throw new Error("the browser could not encode this image");
  return out.buffer;
}
