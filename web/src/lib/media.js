// Client-side media policy: what a file IS (magic bytes), what gets converted,
// and what this browser can play. An upload is stored exactly as it is sent, so
// every image and every recording is normalized here, the moment it is
// attached:
//
//   image -> WebP at 75% quality, longest side capped at 1280px (but never
//            shrunk by more than 2x — see scaleToFit)
//   audio -> WebM/Opus, 24 kHz, mono, 48 kbps
//
// internal/tools/image.go runs that same image scheme on the server for a
// picture create_file is handed, so both paths land in the chat alike.
//
// Two browser facts shape the code:
//
// 1. Safari — every version, desktop and iOS — cannot ENCODE WebP. toBlob()
//    silently hands back a PNG instead of throwing, so the result's own
//    blob.type decides the filename, never the type we asked for. The server
//    accepts both.
// 2. Audio goes through WebCodecs (inside mediabunny, which is pure TypeScript
//    and carries no WASM). AudioEncoder exists in Chrome/Edge 94+, Firefox
//    desktop 130+ and Safari 26+ only, and WebCodecs needs a secure context,
//    so a plain-HTTP LAN origin has none either. There is no WASM-free Opus
//    encoder to fall back on, so those cases get a clear refusal instead of a
//    silently broken file.

// Longest side kept for an image, whatever its orientation.
const MAX_SIDE = 1280;
const IMAGE_QUALITY = 0.75;

// sniffKind classifies a file by its magic bytes: "image", "audio", "pdf" or ""
// (anything else is judged by content — see lib/text-sniff.js). The rule the
// server applies in internal/attach, so an extension-less or misnamed file goes
// by what it IS and the two sides cannot disagree. ISO-BMFF (mp4/mov) is
// deliberately not recognized: the app has no video player. So is ICO — no
// conversion path handles it, so an .ico is refused here and downloads there.
// An EBML file counts as audio whatever its DocType — the conversion below
// either pulls out a soundtrack or fails with a clear error.
export async function sniffKind(file) {
  const b = new Uint8Array(await file.slice(0, 16).arrayBuffer());
  const at = (off, s) => {
    for (let i = 0; i < s.length; i++) if (b[off + i] !== s.charCodeAt(i)) return false;
    return true;
  };
  if (at(0, "\x89PNG") || at(0, "\xff\xd8\xff") || at(0, "GIF8")) return "image";
  if (at(0, "RIFF") && at(8, "WEBP")) return "image";
  // "BM" is two bytes of weak signature, so the four reserved zeroes that
  // follow a BMP's file size are part of the check.
  if (at(0, "BM") && b[6] === 0 && b[7] === 0 && b[8] === 0 && b[9] === 0) return "image";
  if (at(0, "%PDF")) return "pdf";
  if (at(0, "ID3") || at(0, "OggS") || at(0, "fLaC") || at(0, "\x1aE\xdf\xa3")) return "audio";
  if (at(0, "RIFF") && at(8, "WAVE")) return "audio";
  if (b[0] === 0xff && (b[1] & 0xe0) === 0xe0) return "audio"; // bare MP3 frame, no ID3 tag
  return "";
}

// extFromMime is the display suffix for a mime, for a file whose name has none.
export const extFromMime = (mime) =>
  (mime?.split("/")[1] ?? "").split(";")[0].replace(/^x-/, "");

// isAudio picks the attachments that want a player; canPlayAudio is the gate on
// actually showing one — a player for a codec this browser cannot decode is a
// dead button, so an unsupported recording falls back to the download chip.
// Cached per mime: canPlayType is a DOM call and the list re-derives per render.
export const isAudio = (att) => att?.mime?.startsWith("audio/");
const playable = new Map();
export function canPlayAudio(att) {
  const mime = att?.mime ?? "";
  if (!playable.has(mime)) {
    playable.set(mime, document.createElement("audio").canPlayType(mime) !== "");
  }
  return playable.get(mime);
}

// renamed wraps a converted blob in a File whose name and type match what the
// browser actually produced — on Safari a WebP request yields a PNG, and the
// name has to say so.
function renamed(file, blob, fallback) {
  const base = (file.name || fallback).replace(/\.[^.]+$/, "");
  return new File([blob], `${base || fallback}.${extFromMime(blob.type) || "bin"}`, {
    type: blob.type,
    lastModified: Date.now(),
  });
}

// scaleToFit caps the LONGEST side at max, keeping the aspect ratio: a
// landscape image is capped by width, a portrait one by height, and anything
// already smaller is left alone (no upscaling). The cap yields to a 2x floor —
// past that, shrinking a 5000px photo to 1280 throws away more detail than it
// saves bytes, so it stops at half size instead.
export function scaleToFit(w, h, max) {
  const long = Math.max(w, h);
  if (long <= max) return { width: w, height: h };
  const k = Math.max(max, long / 2) / long;
  return { width: Math.max(1, Math.round(w * k)), height: Math.max(1, Math.round(h * k)) };
}

// convertImage decodes any supported image, downscales it if it is bigger than
// MAX_SIDE on its longest side, and re-encodes to WebP (PNG on Safari). EXIF
// orientation needs no code: createImageBitmap applies it by default.
export async function convertImage(file) {
  const bitmap = await createImageBitmap(file);
  try {
    const { width, height } = scaleToFit(bitmap.width, bitmap.height, MAX_SIDE);
    const canvas = document.createElement("canvas");
    canvas.width = width;
    canvas.height = height;
    canvas.getContext("2d").drawImage(bitmap, 0, 0, width, height);
    const blob = await new Promise((resolve, reject) =>
      canvas.toBlob(
        (b) => (b ? resolve(b) : reject(new Error("the browser could not encode this image"))),
        "image/webp",
        IMAGE_QUALITY,
      ),
    );
    return renamed(file, blob, "image");
  } finally {
    bitmap.close();
  }
}

// convertAudio transcodes any supported recording to WebM/Opus, 24 kHz mono at
// 48 kbps — small enough to resend on every turn, and the one audio container
// the server accepts. Throws a readable message when the browser has no Opus
// encoder, which is the common failure (see the note at the top).
export async function convertAudio(file) {
  // Imported here, not at the top: ~1 MB of muxers that only an audio
  // attachment ever needs, kept out of the main bundle.
  const mb = await import("mediabunny");
  const audio = {
    codec: "opus",
    numberOfChannels: 1,
    sampleRate: 24000,
    quality: new mb.Quality({ bitrate: 48000 }),
    // The input may already be Opus in WebM; without this it would be copied
    // through untouched, keeping its old sample rate and channel count.
    forceTranscode: true,
  };
  if (!(await mb.canEncodeAudio("opus", audio))) {
    throw new Error(
      "this browser can't convert audio (needs Chrome, Firefox on desktop, or Safari 26+, over HTTPS or localhost)",
    );
  }
  const input = new mb.Input({ source: new mb.BlobSource(file), formats: mb.ALL_FORMATS });
  const output = new mb.Output({
    format: new mb.WebMOutputFormat(),
    target: new mb.BufferTarget(),
  });
  const conversion = await mb.Conversion.init({
    input,
    output,
    video: { discard: true }, // a video file's picture is not ours to keep
    audio,
  });
  if (!conversion.isValid) {
    throw new Error(`no audio track could be read (${conversion.discardedTracks.map((t) => t.reason).join(", ") || "unsupported file"})`);
  }
  await conversion.execute();
  const buffer = output.target.buffer;
  if (!buffer?.byteLength) throw new Error("the conversion produced no audio");
  return renamed(file, new Blob([buffer], { type: "audio/webm" }), "audio");
}
