// Client-side media policy: what a file IS (magic bytes), what gets converted,
// and what this browser can play. An upload is stored exactly as it is sent, so
// every image and every recording is normalized here, the moment it is
// attached:
//
//   image -> PNG, longest side capped at 1280px (but never shrunk by more than
//            2x — see scaleToFit), and quantized to 256 colours only when the
//            server's image_quantization setting is on
//   audio -> MP3, 16 kHz, mono, 32 kbps
//
// internal/tools/image.go runs that same scheme on the server for a picture
// create_file is handed, and the same image_quantization setting governs both
// — except that a picture with soft alpha stays lossless there, since a palette
// has one alpha per colour and quantizing one fringes it.
//
// Both image encoders are always there — the browser's own toBlob and, when
// quantization is on, lib/png-enc.js — so the image path needs no fallback.
// Audio nearly is: no browser can ENCODE mp3, so @mediabunny/mp3-encoder brings
// its own (LAME as WASM, running in a worker), and WASM is everywhere. What can
// still be missing is the DECODE of the source — mediabunny drives WebCodecs'
// AudioDecoder for a compressed recording, and WebCodecs exists in Chrome/Edge
// 94+, Firefox 130+ and Safari 16.4+ only, and needs a secure context. So a
// plain-HTTP LAN origin converts a WAV and refuses an m4a, with a clear error
// rather than a silently broken file.

// Longest side kept for an image, whatever its orientation.
const MAX_SIDE = 1280;

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
// audio/mpeg is the one media mime whose subtype is not its extension.
const suffixAlias = { mpeg: "mp3" };
export const extFromMime = (mime) => {
  const sub = (mime?.split("/")[1] ?? "").split(";")[0].replace(/^x-/, "");
  return suffixAlias[sub] ?? sub;
};

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
// conversion actually produced — the suffix is derived from the blob, never
// from the type that was asked for.
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
// MAX_SIDE on its longest side, and re-encodes to PNG — lossless, or quantized
// to an indexed 256 colours (several times smaller) when quantize is on. EXIF
// orientation needs no code: createImageBitmap applies it by default.
export async function convertImage(file, quantize = false) {
  const bitmap = await createImageBitmap(file);
  try {
    const { width, height } = scaleToFit(bitmap.width, bitmap.height, MAX_SIDE);
    const canvas = document.createElement("canvas");
    canvas.width = width;
    canvas.height = height;
    const ctx = canvas.getContext("2d");
    ctx.drawImage(bitmap, 0, 0, width, height);
    let blob;
    if (quantize) {
      // Imported here, not at the top: only a quantized attach fetches it.
      const { encodePNG } = await import("./png-enc.js");
      blob = new Blob([encodePNG(ctx.getImageData(0, 0, width, height))], {
        type: "image/png",
      });
    } else {
      blob = await new Promise((resolve, reject) =>
        canvas.toBlob(
          (b) => (b ? resolve(b) : reject(new Error("the browser could not encode this image"))),
          "image/png",
        ),
      );
    }
    return renamed(file, blob, "image");
  } finally {
    bitmap.close();
  }
}

// convertAudio transcodes any supported recording to MP3, 16 kHz mono at
// 32 kbps — the rate a transcription model resamples to anyway, so no bit is
// spent on what it would discard — and the one audio container an upload may
// carry. Throws a readable message when the source cannot be decoded.
export async function convertAudio(file) {
  // Imported here, not at the top: ~1 MB of muxers plus a ~310 kB WASM LAME
  // that only an audio attachment ever needs, kept out of the main bundle.
  const mb = await import("mediabunny");
  const audio = {
    codec: "mp3",
    numberOfChannels: 1,
    sampleRate: 16000,
    quality: new mb.Quality({ bitrate: 32000 }),
    // The input may already be MP3; without this it would be copied through
    // untouched, keeping its old sample rate, channel count and bitrate.
    forceTranscode: true,
  };
  // The support check is also the register-once guard: after the first call
  // mediabunny answers true and the WASM is neither fetched nor registered
  // again.
  if (!(await mb.canEncodeAudio("mp3", audio))) {
    const { registerMp3Encoder } = await import("@mediabunny/mp3-encoder");
    registerMp3Encoder();
  }
  const input = new mb.Input({ source: new mb.BlobSource(file), formats: mb.ALL_FORMATS });
  const output = new mb.Output({
    format: new mb.Mp3OutputFormat(),
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
  return renamed(file, new Blob([buffer], { type: "audio/mpeg" }), "audio");
}
