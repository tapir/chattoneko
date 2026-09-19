// Long-press action sheet state — the Share / Copy drawer that opens on an
// image thumbnail or a file chip in the chat. Module-level singleton like
// `viewer` (same shape, same reason): the triggers live deep in MessageItem /
// ImageGallery, the sheet is mounted once in App.svelte.
//
// Attachment shape is the server's AttachmentMeta ({id, filename, kind, mime,
// size, …}); nothing but `id`, `filename` and `kind` is required.

class AttachMenuState {
  attachment = $state(null);

  open(attachment) {
    // A staged, still-uploading file (the pending send bubble) carries a
    // local `pending-*` id and no server copy yet: nothing to fetch.
    if (!attachment || attachment.file) return;
    this.attachment = attachment;
  }

  close() {
    this.attachment = null;
  }
}

export const attachMenu = new AttachMenuState();
