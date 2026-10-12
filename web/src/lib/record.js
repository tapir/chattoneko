// Push-to-talk capture: getUserMedia + MediaRecorder → a File the ordinary
// staging path (`app.addAttachments`) accepts, so a recording is handled
// exactly like a file the user picked.
//
// Both APIs exist only in a secure context. A page served over plain HTTP on a
// LAN has no `navigator.mediaDevices` at all — nothing to polyfill — so the
// composer keys its mic button on micAvailable() (docs/frontend.md → Composer).
//
// The blob is named from its own mime through media.js's extFromMime, the same
// helper the native pickers name theirs with: the server classifies by
// EXTENSION, so whatever this browser recorded (webm on Chromium, mp4 on
// Safari, ogg on Firefox — all three on attach.audioExts) has to arrive with a
// suffix that list holds.

import { extFromMime } from './media.js';

// What the blob is named if the browser reports no mimeType at all; ffmpeg probes
// the CONTENT server-side (internal/media), so the suffix only has to be one the
// server's extension list knows.
const FALLBACK_MIME = 'audio/webm';

export const micAvailable = () =>
  typeof navigator !== 'undefined' &&
  typeof MediaRecorder !== 'undefined' &&
  typeof navigator.mediaDevices?.getUserMedia === 'function';

// startRecording resolves once the mic is live; until then the caller has
// nothing to stop and should just drop its pending press.
export async function startRecording() {
  const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
  const chunks = [];
  const rec = new MediaRecorder(stream);
  let settled = null;
  const ended = new Promise((resolve) => (settled = resolve));
  rec.ondataavailable = (e) => {
    if (e.data.size) chunks.push(e.data);
  };
  // stop() and a failure alike land here: a recorder error resolves rather than
  // rejects, because this also runs from an unmount teardown where a rejection
  // would be unhandled. Nothing arrived → a zero-byte blob → stop() returns
  // null and the caller reports it.
  rec.onstop = () => settled();
  rec.onerror = () => settled();
  rec.start();

  const release = async () => {
    if (rec.state !== 'inactive') {
      rec.stop();
      await ended;
    }
    // The platform's recording indicator has to go out on every path.
    stream.getTracks().forEach((t) => t.stop());
  };

  return {
    // stop resolves the recording as a File, or null when nothing was captured.
    async stop() {
      await release();
      const blob = new Blob(chunks, { type: rec.mimeType || FALLBACK_MIME });
      if (!blob.size) return null;
      return new File([blob], `recording.${extFromMime(blob.type) || 'webm'}`, {
        type: blob.type,
      });
    },
    cancel: release,
  };
}
