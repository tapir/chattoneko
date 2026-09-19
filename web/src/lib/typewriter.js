// Adaptive typewriter: incoming deltas are buffered and drained onto
// `display` once per animation frame. Small backlogs drain a few chars per
// frame (smooth letter-by-letter); the pace scales with backlog so bursts
// never lag far behind, and very large backlogs (reconnect replay) catch
// up within ~11 frames (~180ms at 60fps).

export class Typewriter {
  constructor(onUpdate) {
    this.buffer = "";
    this.display = "";
    this.onUpdate = onUpdate;
    this.raf = null;
  }

  push(text) {
    if (!text) return;
    this.buffer += text;
    this.schedule();
  }

  schedule() {
    if (this.raf == null) {
      this.raf = requestAnimationFrame(() => this.tick());
    }
  }

  tick() {
    this.raf = null;
    if (this.buffer.length === 0) return;
    const len = this.buffer.length;
    let n = 2 + Math.ceil(len / 60); // steady pace + mild backlog pressure
    if (len > 1500) n = Math.ceil(len / 11); // big backlog: catch up fast
    if (n > len) n = len;
    const chunk = this.buffer.slice(0, n);
    this.buffer = this.buffer.slice(n);
    this.display += chunk;
    this.onUpdate(this.display);
    if (this.buffer.length > 0) this.schedule();
  }

  flush() {
    if (this.raf != null) {
      cancelAnimationFrame(this.raf);
      this.raf = null;
    }
    if (this.buffer.length > 0) {
      this.display += this.buffer;
      this.buffer = "";
    }
    this.onUpdate(this.display);
  }

  destroy() {
    if (this.raf != null) {
      cancelAnimationFrame(this.raf);
      this.raf = null;
    }
  }
}
