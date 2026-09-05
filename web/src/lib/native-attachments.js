// Native attachment pickers for the Capacitor (Android) build. Each helper
// opens a native UI, waits for the result, and returns File[] ready for
// app.addAttachments(). An empty array means the user cancelled; genuine
// failures throw so the caller can toast. Plugin imports are dynamic so the
// plain web bundle never evaluates them (and desktop never loads the code
// until a native platform actually calls in).

import { Capacitor } from "@capacitor/core";

// MIME filter for the Files picker — mirrors the image + text/code formats
// in state.svelte.js (IMAGE_EXTS/TEXT_EXTS). text/* covers plain-text
// extensions on most devices; the explicit application/* entries cover
// structured formats Android doesn't classify as text. Some code extensions
// (.go/.rs/.sh…) have no registered MIME type and may stay unselectable in
// some file managers — addAttachments() revalidates by extension regardless,
// so nothing unsupported can slip through.
const FILE_MIME_TYPES = [
  "image/jpeg",
  "image/png",
  "image/gif",
  "image/webp",
  "text/*",
  "application/json",
  "application/ld+json",
  "application/xml",
  "application/javascript",
  "application/typescript",
  "application/x-yaml",
  "application/yaml",
  "application/toml",
  "application/sql",
  "application/x-sh",
];

const MIME_EXT = {
  "image/jpeg": "jpg",
  "image/png": "png",
  "image/gif": "gif",
  "image/webp": "webp",
};

// Plugin rejections for a dismissed picker: the camera plugin tags them
// with OS-PLUG-CAMR-* codes (CameraErrorCode), the file picker (and the
// web fallbacks) only set the message. Treat both as a clean cancel.
const CANCEL_CODES = new Set([
  "OS-PLUG-CAMR-0006", // TakePhotoCancelled
  "OS-PLUG-CAMR-0013", // EditPhotoCancelled
  "OS-PLUG-CAMR-0020", // ChooseMediaCancelled
]);

function isCancel(e) {
  return CANCEL_CODES.has(e?.code) || /cancel/i.test(e?.message ?? "");
}

// Filename stamp: camera-20260904115830.jpg — readable and collision-safe
// enough for back-to-back picks in one chat.
function stamp() {
  return new Date().toISOString().slice(0, 19).replace(/[T:]/g, "");
}

async function fetchBlob(url, what) {
  const res = await fetch(url);
  if (!res.ok) throw new Error(`couldn't read ${what} (HTTP ${res.status})`);
  return res.blob();
}

// Camera/gallery results have no filename — synthesize one from the blob's
// MIME type so the extension passes addAttachments() validation.
function photoFile(blob, prefix, suffix = "") {
  const ext = MIME_EXT[blob.type] ?? "jpg";
  return new File([blob], `${prefix}-${stamp()}${suffix}.${ext}`, {
    type: blob.type || "image/jpeg",
  });
}

// Camera/gallery results: webPath is a WebView-fetchable URL on every
// platform; fall back to the raw file URI routed through the bridge.
function mediaUrl(media) {
  if (media.webPath) return media.webPath;
  if (media.uri) return Capacitor.convertFileSrc(media.uri);
  throw new Error("native picker returned no file");
}

// Camera: launches the device camera app; the shot comes back as a single
// JPEG. The drawer is already closed by the caller before this runs.
export async function capturePhoto() {
  const { Camera } = await import("@capacitor/camera");
  try {
    const photo = await Camera.takePhoto({
      quality: 90,
      correctOrientation: true,
    });
    return [photoFile(await fetchBlob(mediaUrl(photo), "camera photo"), "camera")];
  } catch (e) {
    if (isCancel(e)) return [];
    throw e;
  }
}

// Photos: opens the system photo picker limited to still images (no
// videos). Multiple selection allowed; per-file and count limits are
// enforced by addAttachments().
export async function pickPhotos() {
  const { Camera, MediaTypeSelection } = await import("@capacitor/camera");
  try {
    const { results } = await Camera.chooseFromGallery({
      mediaType: MediaTypeSelection.Photo, // images only — no video files
      allowMultipleSelection: true,
    });
    const out = [];
    let seq = 0;
    for (const media of results) {
      const suffix = results.length > 1 ? `-${++seq}` : "";
      out.push(
        photoFile(await fetchBlob(mediaUrl(media), "gallery photo"), "photo", suffix),
      );
    }
    return out;
  } catch (e) {
    if (isCancel(e)) return [];
    throw e;
  }
}

// Files: opens the system file manager (SAF) filtered to the attachable
// formats. Picked files keep their real names, so extension validation in
// addAttachments() matches what the server will enforce at send time.
export async function pickFiles() {
  const { FilePicker } = await import("@capawesome/capacitor-file-picker");
  try {
    const { files } = await FilePicker.pickFiles({ types: FILE_MIME_TYPES });
    const out = [];
    for (const f of files) {
      if (!f.path) continue;
      const blob = await fetchBlob(Capacitor.convertFileSrc(f.path), f.name);
      out.push(
        new File([blob], f.name || "file", {
          type: blob.type || f.mimeType || "",
        }),
      );
    }
    return out;
  } catch (e) {
    if (isCancel(e)) return [];
    throw e;
  }
}
