// Share + copy an attachment, shared by the lightbox's share button and the
// chat's long-press sheet. Both need the bytes, and the two platforms get at
// them differently:
//
//   share — native: Capacitor's Share plugin wants a local file:// URL, so
//     the bytes are staged in the app cache dir first (already exposed to
//     other apps by the FileProvider in
//     mobile/android/app/src/main/res/xml/file_paths.xml). Web: the Web Share
//     API takes a File straight from the blob — but only in a secure context,
//     so a plain-http LAN address has neither path and says so.
//   copy — text attachments only: they copy their contents (the server serves
//     them as text/plain). An image gets no Copy row, so the async Clipboard
//     API is out of this file.
//
// Failures report themselves as toasts and return false, so callers are
// one-liners; dismissing the system share sheet is not a failure.

import { api } from './api.js';
import { app } from './state.svelte.js';
import { isNative } from './server.js';
import { copyText } from './clipboard.js';

// Where the bytes come from: a staged local preview if there still is one,
// otherwise the server copy (attachmentUrl appends ?token= — an <img> or a
// bare fetch cannot carry the Authorization header).
async function fetchBlob(att) {
  // no-store: an <img> already on screen caches the same URL without CORS
  // headers, and a poisoned cache entry makes this fail ("Failed to fetch")
  // for a picture that is visibly there. Servers send Vary: Origin now, which
  // fixes it for good — this keeps working against older ones.
  const res = await fetch(att.previewUrl || api.attachmentUrl(att.id), { cache: 'no-store' });
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  return res.blob();
}

export async function shareAttachment(att) {
  try {
    const blob = await fetchBlob(att);
    const name = att.filename || 'attachment';
    if (!isNative()) {
      if (!navigator.share) throw new Error('sharing needs the app or an HTTPS page');
      const file = new File([blob], name, { type: blob.type || att.mime || '' });
      if (navigator.canShare && !navigator.canShare({ files: [file] }))
        throw new Error('this browser cannot share files');
      await navigator.share({ files: [file], title: name });
      return true;
    }
    const { Filesystem, Directory } = await import('@capacitor/filesystem');
    const { Share } = await import('@capacitor/share');
    // The extension is what gives the share sheet its MIME type; stripping
    // separators keeps a hostile filename inside the cache dir.
    const path = name.replace(/[^\w.-]+/g, '_');
    await Filesystem.writeFile({
      path,
      data: await blobBase64(blob),
      directory: Directory.Cache,
      recursive: true, // the plugin's cache folder may not exist yet
    });
    const { uri } = await Filesystem.getUri({ path, directory: Directory.Cache });
    await Share.share({ files: [uri] });
    return true;
  } catch (e) {
    if (!dismissed(e)) app.toast('error', `Couldn't share: ${e?.message ?? 'unknown error'}`);
    return false;
  }
}

export async function copyAttachment(att) {
  try {
    // These files are usually code — copy the text, not a URL to it.
    if (!(await copyText(await api.attachmentText(att.id))))
      throw new Error('the clipboard refused the copy');
    app.toast('success', `Copied ${att.filename}`);
    return true;
  } catch (e) {
    app.toast('error', `Couldn't copy: ${e?.message ?? 'unknown error'}`);
    return false;
  }
}

// Filesystem's binary write path takes base64 (no `encoding`); FileReader is
// the only portable encoder and the data-URL prefix isn't part of it.
// ponytail: the whole file crosses the bridge as base64 — fine at the sizes
// the server hands back (images are re-encoded to <=2048px); swap in
// Filesystem.downloadFile, which streams natively, if that ever hurts.
function blobBase64(blob) {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onloadend = () => resolve(String(r.result).split(',')[1] ?? '');
    r.onerror = () => reject(new Error('could not read the file'));
    r.readAsDataURL(blob);
  });
}

const dismissed = (e) => /cancel|abort/i.test(`${e?.name ?? ''} ${e?.message ?? ''}`);
