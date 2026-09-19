// What an attachment IS, by the extension on its name. Nothing here reads
// bytes: the server runs the same extension check and converts every picture
// to a JPEG and every recording to a mono MP3 before it stores anything
// (internal/attach + internal/media), so a file that lies about its suffix is
// rejected there and reported as a toast here.
//
// Anything not on these lists is judged by content (lib/text-sniff.js), so
// there is still no text-extension list to keep in sync.

// The accepted media, the same set the server's attach.imageExts/audioExts
// hold. A video container counts as audio: no picture survives the conversion,
// only the soundtrack.
const IMAGE_EXTS = new Set(["png", "bmp", "tga", "jpg", "jpeg", "gif", "webp"]);
const AUDIO_EXTS = new Set([
  "wav", "mp3", "ogg", "oga", "opus", "webm", "mkv", "mov",
  "flac", "alac", "m4a", "m4b", "mp4", "aac",
]);

// The server trims a filename before it reads the suffix (attach.CleanFilename),
// so the client has to as well or the two disagree on "photo.png ".
const extOf = (name) => /\.([a-z0-9]+)$/.exec((name ?? "").trim().toLowerCase())?.[1] ?? "";

// kindOfExt: "image" | "audio" | "pdf" | "" — and "" is what goes on to the
// text sniff.
export function kindOfExt(name) {
  const ext = extOf(name);
  if (IMAGE_EXTS.has(ext)) return "image";
  if (AUDIO_EXTS.has(ext)) return "audio";
  return ext === "pdf" ? "pdf" : "";
}

// previewsLocally: whether a staged file can be painted from its own bytes.
// TGA is the one accepted picture no browser decodes, so its chip shows the
// filename instead and the message paints the server's JPEG once it exists.
export const previewsLocally = (name) => extOf(name) !== "tga";

// isAudio picks the attachments that want a player; canPlayAudio is the gate on
// actually showing one. Every recording this app stores is an MP3, which
// everything plays — but a row a tool fetched can still be an ogg or a flac,
// and a player for a codec this browser cannot decode is a dead button, so an
// unsupported one falls back to the download chip.
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

// extFromMime is the display suffix for a mime, for a file whose name has none
// (the native pickers' blobs). audio/mpeg is the one media mime whose subtype
// is not its extension.
const suffixAlias = { mpeg: "mp3" };
export const extFromMime = (mime) => {
  const sub = (mime?.split("/")[1] ?? "").split(";")[0].replace(/^x-/, "");
  return suffixAlias[sub] ?? sub;
};
