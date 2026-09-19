// Native attachment pickers for the Capacitor (Android) build. Each helper
// opens a native UI, waits for the result, and returns File[] ready for
// app.addAttachments(). An empty array means the user cancelled; genuine
// failures throw so the caller can toast. Plugin imports are dynamic so the
// plain web bundle never evaluates them (and desktop never loads the code
// until a native platform actually calls in).

import { Capacitor } from "@capacitor/core";
import { extFromMime } from "./media.js";

// MIME filter for the Files picker: none. Android reports application/
// octet-stream for every extension it can't map (.go, .rs, .sh, .toml…), so
// any allow-list either hides those files or includes the catch-all and
// filters nothing. addAttachments() classifies by extension and content, so
// the picker stays out of it.

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

// Camera shots have no filename — synthesize one, suffixed from the blob's MIME
// type so the user sees something readable.
function photoFile(blob, prefix) {
  return new File([blob], `${prefix}-${stamp()}.${extFromMime(blob.type) || "jpg"}`, {
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
// CAPTURE_SIDE is the box the plugin fits the shot into, so it caps the LONG
// side at 1920 in either orientation — which is what the server's conversion
// keeps for a landscape picture, and more than it keeps for a portrait one
// (ffmpeg caps that at 1080 wide and trims the rest). One box cannot match both:
// the plugin needs its two sizes up front, while the stock camera app picks the
// shutter orientation after ours has run, so a landscape box would come back
// smaller than the server keeps whenever the phone gets turned. Over-delivering
// costs a few hundred KB of upload; under-delivering loses detail for good.
// Both target options are required — the plugin ignores a lone value; aspect is
// preserved. Gallery picks skip this: they come through the photo picker as the
// original bytes, never re-encoded.
const CAPTURE_SIDE = 1920;

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

// The picker's display name when it carries an extension — which is what
// classification reads now, so a name without one has to be given the suffix
// the blob's own mime implies. The plugin falls back to the URI's last path
// segment ("12") when a provider has no DISPLAY_NAME.
function pickedName(f, blob) {
  const name = f.name ?? "";
  if (/\.[A-Za-z0-9]{2,5}$/.test(name)) return name;
  const ext = extFromMime(blob.type);
  return `photo-${stamp()}${ext ? "." + ext : ""}`;
}

// Photos: the system photo picker (AndroidX PickMultipleVisualMedia) — images
// only, multi-select, original bytes; per-file and count limits are enforced
// by addAttachments().
//
// Deliberately NOT @capacitor/camera's chooseFromGallery: that routes through
// Ionic's ioncameralib, which starts a translucent trampoline activity (a
// first-run flicker) and then covers the screen with its own dark spinner
// overlay while it copies the pick. The file-picker plugin the Files row uses
// goes straight to the system picker.
export function pickPhotos() {
  return pickVia((FilePicker) => FilePicker.pickImages(), "gallery photo");
}

// Files: opens the system file manager (SAF), unfiltered — see the note above.
export function pickFiles() {
  return pickVia((FilePicker) => FilePicker.pickFiles(), "file");
}
