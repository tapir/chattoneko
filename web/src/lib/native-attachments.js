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
// structured formats Android doesn't classify as text.
//
// octet-stream is what Android reports for any extension it can't map (.go,
// .rs, .sh, .toml…) — file-picker 8.1.0 started honouring `types` on
// multi-select picks, so without it those code files became unselectable.
// addAttachments() revalidates by extension regardless, so nothing
// unsupported can slip through.
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
  "application/octet-stream",
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

// Camera shots have no filename — synthesize one from the blob's MIME type so
// the extension passes addAttachments() validation.
function photoFile(blob, prefix) {
  const ext = MIME_EXT[blob.type] ?? "jpg";
  return new File([blob], `${prefix}-${stamp()}.${ext}`, {
    type: blob.type || "image/jpeg",
  });
}

// Camera results: webPath is a WebView-fetchable URL on every platform; fall
// back to the raw file URI routed through the bridge.
function mediaUrl(media) {
  if (media.webPath) return media.webPath;
  if (media.uri) return Capacitor.convertFileSrc(media.uri);
  throw new Error("native picker returned no file");
}

// Camera: launches the device camera app; the shot comes back as a single
// JPEG. The drawer is already closed by the caller before this runs.
//
// CAPTURE_SIDE caps the long side: the stock camera app returns a full-sensor
// photo (several MB) the server downscales to 2048px anyway. Both target
// options are required — the plugin ignores a lone value; aspect is preserved.
// Gallery picks skip this: they come through the photo picker as the original
// bytes, never re-encoded.
const CAPTURE_SIDE = 1280;

export async function capturePhoto() {
  const { Camera } = await import("@capacitor/camera");
  try {
    const photo = await Camera.takePhoto({
      quality: 90,
      correctOrientation: true,
      targetWidth: CAPTURE_SIDE,
      targetHeight: CAPTURE_SIDE,
    });
    return [photoFile(await fetchBlob(mediaUrl(photo), "camera photo"), "camera")];
  } catch (e) {
    if (isCancel(e)) return [];
    throw e;
  }
}

// Shared picker loop: run a @capawesome/capacitor-file-picker call, read each
// result through the bridge, hand back File[] for addAttachments(). An empty
// array means the user cancelled; genuine failures throw.
async function pickVia(pick, what) {
  const { FilePicker } = await import("@capawesome/capacitor-file-picker");
  try {
    const { files } = await pick(FilePicker);
    const out = [];
    for (const f of files) {
      if (!f.path) continue;
      const blob = await fetchBlob(Capacitor.convertFileSrc(f.path), f.name || what);
      out.push(
        new File([blob], pickedName(f, blob), {
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

// The picker's display name when it carries an extension, so validation in
// addAttachments() matches what the server enforces at send time. The plugin
// falls back to the URI's last path segment ("12") when a provider has no
// DISPLAY_NAME — synthesize an extension from the MIME in that case.
function pickedName(f, blob) {
  const name = f.name ?? "";
  if (/\.[A-Za-z0-9]{2,5}$/.test(name)) return name;
  return `photo-${stamp()}.${MIME_EXT[blob.type] ?? "jpg"}`;
}

// Photos: the system photo picker (AndroidX PickMultipleVisualMedia) — images
// only, multi-select, original bytes; per-file and count limits are enforced
// by addAttachments().
//
// Deliberately NOT @capacitor/camera's chooseFromGallery: that routes through
// Ionic's ioncameralib, which starts a translucent trampoline activity (the
// first-run flicker) and then covers the screen with its own dark spinner
// overlay while it copies the pick. The camera plugin's other gallery path
// (pickImages) is deprecated, so the photo picker comes from the file-picker
// plugin the Files row already uses — no new dependency.
export function pickPhotos() {
  return pickVia((FilePicker) => FilePicker.pickImages(), "gallery photo");
}

// Files: opens the system file manager (SAF) filtered to the attachable
// formats.
export function pickFiles() {
  return pickVia(
    (FilePicker) => FilePicker.pickFiles({ types: FILE_MIME_TYPES }),
    "file",
  );
}
