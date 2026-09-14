// Client-side media conversion. This file IS the app's codec policy: the
// server no longer decodes a single pixel or sample (internal/attach only
// sniffs magic bytes and stores what it gets), so every image and every
// recording is normalized here, the moment it is attached:
//
//   image -> WebP at 75% quality, longest side capped at 1280px (but never
//            shrunk by more than 2x — see scaleToFit)
//   audio -> WebM/Opus, 24 kHz, mono, 48 kbps
//
// Two browser facts shape the code:
//
// 1. Safari — every version, desktop and iOS — cannot ENCODE WebP. toBlob()
//    silently hands back a PNG instead of throwing, so the result's own
//    blob.type decides the filename, never the type we asked for. The server
//    accepts both, which is the only reason this is a non-event.
// 2. Audio goes through WebCodecs (inside mediabunny, which is pure TypeScript
//    and carries no WASM). AudioEncoder exists in Chrome/Edge 94+, Firefox
//    desktop 130+ and Safari 26+ only — Firefox for Android and Safari 18 and
//    older have no audio encoder at all, and WebCodecs needs a secure context,
//    so a plain-HTTP LAN origin has none either. There is no WASM-free Opus
//    encoder to fall back on, so those cases get a clear refusal instead of a
//    silently broken file.

// Longest side kept for an image, whatever its orientation.
const MAX_SIDE = 1280;
const IMAGE_QUALITY = 0.75;

// What the browser can decode (createImageBitmap) and turn into WebP. TIFF is
// deliberately absent: no engine decodes it, and the server no longer does.
const IMAGE_EXTS = ["png", "jpg", "jpeg", "webp", "bmp", "gif", "ico"];
// What mediabunny can read: MP4/AAC, MP3, WebM and Ogg (Opus or Vorbis), WAV,
// FLAC. Everything leaves as WebM/Opus.
const AUDIO_EXTS = ["mp4", "m4a", "mp3", "webm", "ogg", "oga", "opus", "wav", "flac"];

const extOf = (name) =>
  name.includes(".") ? name.slice(name.lastIndexOf(".") + 1).toLowerCase() : "";

// mediaKind classifies a staged file by its name: "image", "audio", "pdf" or
// "" (anything else is judged by content — see lib/text-sniff.js). Extensions
// are the client's hint only; the server re-decides from the bytes.
export function mediaKind(filename) {
  const ext = extOf(filename || "");
  if (IMAGE_EXTS.includes(ext)) return "image";
  if (AUDIO_EXTS.includes(ext)) return "audio";
  return ext === "pdf" ? "pdf" : "";
}

// renamed wraps a converted blob in a File whose name and type match what the
// browser actually produced — on Safari a WebP request yields a PNG, and the
// name has to say so.
function renamed(file, blob, fallback) {
  const base = (file.name || fallback).replace(/\.[^.]+$/, "");
  const ext = blob.type.split("/")[1]?.replace(/^x-/, "") || "bin";
  return new File([blob], `${base || fallback}.${ext}`, {
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
// orientation needs no code: createImageBitmap applies it by default, which is
// what the server used to do by hand for JPEGs.
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
