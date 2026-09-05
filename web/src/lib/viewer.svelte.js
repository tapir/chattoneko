// Attachment viewer overlay state — the fullscreen text/image lightbox that
// opens when an attachment in a message is clicked. Module-level singleton
// like `app`: MessageItem calls viewer.open(att, items), App.svelte renders
// the overlay. Attachment shape is the server's AttachmentMeta ({id,
// filename, kind, mime, size, …}); nothing but `id`, `filename` and `kind`
// is required.
//
// `items` is the optional GALLERY SET the opened attachment belongs to —
// every image attachment of the same message. With more than one member the
// viewer gains prev/next: swiping left/right, arrow keys and the side
// chevrons all just move `attachment` within `items`, so App.svelte re-passes
// a prop and the overlay (which is never keyed) stays mounted. Text files
// and lone images open with no set and therefore no navigation.

class ViewerState {
  attachment = $state(null);
  items = $state(null);

  open(attachment, items = null) {
    this.attachment = attachment;
    // Only a real multi-image set navigates; anything else keeps the
    // overlay's chrome exactly as it was for a single attachment.
    this.items = Array.isArray(items) && items.length > 1 ? items : null;
  }

  // Position of the open attachment in the set, or -1 without one.
  get index() {
    if (!this.items || !this.attachment) return -1;
    return this.items.findIndex((a) => a.id === this.attachment.id);
  }

  // Move `delta` steps through the set, wrapping at both ends: a gallery you
  // had to close and reopen to reach the first picture again is a gallery
  // nobody browses.
  step(delta) {
    const n = this.items?.length ?? 0;
    if (n < 2) return;
    const i = this.index < 0 ? 0 : this.index;
    this.attachment = this.items[(((i + delta) % n) + n) % n];
  }

  next() {
    this.step(1);
  }

  prev() {
    this.step(-1);
  }

  close() {
    this.attachment = null;
    this.items = null;
  }
}

export const viewer = new ViewerState();
