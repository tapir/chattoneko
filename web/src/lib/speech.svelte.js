// Read-aloud playback. The audio is synthesized on demand by POST /api/speech
// and never stored server-side, so the blob URL is revoked as soon as playback
// ends. One message at a time — the same contract AudioPreview keeps for
// recordings, and the reason this is a module singleton rather than per-row
// state: the rows live in MessageItem, the playback outlives any one of them.
import { api } from './api.js';
import { app } from './state.svelte.js';

// id is the message being fetched OR played ('' when idle); loading separates
// the two for the row's button.
export const speech = $state({ id: '', loading: false });

let current = null; // { el, url }

export function stopSpeech() {
  if (current) {
    current.el.pause();
    URL.revokeObjectURL(current.url);
    current = null;
  }
  speech.id = '';
  speech.loading = false;
}

// Starts reading messageId aloud, or stops it when it is already the active
// one. A failure is a toast, not a thrown error: the button is a convenience
// and the message is still on screen to read.
export async function toggleSpeech(messageId) {
  if (speech.id === messageId) {
    stopSpeech();
    return;
  }
  stopSpeech();
  speech.id = messageId;
  speech.loading = true;
  try {
    const url = await api.speech(messageId);
    // Another message was started (or playback stopped) while this was in
    // flight: drop the blob instead of playing it over the newer request.
    if (speech.id !== messageId) {
      URL.revokeObjectURL(url);
      return;
    }
    const el = new Audio(url);
    current = { el, url };
    el.onended = stopSpeech;
    el.onerror = stopSpeech;
    await el.play();
  } catch (e) {
    stopSpeech();
    app.toast('error', `Read aloud failed: ${e?.message || e}`);
  } finally {
    speech.loading = false;
  }
}
