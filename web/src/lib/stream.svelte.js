import { streamUrl } from "./server.js";

// SSE stream manager for one chat. Implements the B4 resume contract:
// events are deduped by seq, reconnects pass ?after=<lastSeq>, and the
// stream reconnects on a slow poll after clean closes (idle/done) so
// generations started from other clients are picked up within seconds.

const RETRY_MS = 1000; // unexpected drop
const POLL_MS = 4000; // clean close (server sent idle/done)

export class ChatStream {
  constructor(chatId, onEvent) {
    this.chatId = chatId;
    this.onEvent = onEvent;
    this.lastSeq = -1;
    this.epoch = null;
    this.es = null;
    this.stopped = false;
    this.cleanClose = false;
    this.timer = null;
    this.connect();
  }

  url() {
    return streamUrl(`/api/chats/${this.chatId}/stream`, { after: this.lastSeq });
  }

  connect() {
    if (this.stopped) return;
    const es = new EventSource(this.url());
    this.es = es;
    es.onmessage = (e) => {
      let ev;
      try {
        ev = JSON.parse(e.data);
      } catch {
        return;
      }
      // The server's seq space resets when the chat hub is pruned and
      // recreated (e.g. after a disconnect on an idle chat); every event
      // carries the hub epoch so we can detect that and reset the dedupe
      // baseline — with a stale lastSeq the entire next generation
      // (deltas, done) would be silently dropped.
      if (ev.epoch && ev.epoch !== this.epoch) {
        this.epoch = ev.epoch;
        this.lastSeq = -1;
      }
      if (typeof ev.seq === "number") {
        if (ev.seq <= this.lastSeq) return; // dedupe replayed events
        this.lastSeq = ev.seq;
      }
      if (ev.type === "done" || ev.type === "idle") this.cleanClose = true;
      this.onEvent(ev);
    };
    es.onerror = () => {
      es.close();
      if (this.stopped) return;
      const delay = this.cleanClose ? POLL_MS : RETRY_MS;
      this.cleanClose = false;
      this.timer = setTimeout(() => this.connect(), delay);
    };
  }

  // Reconnect immediately (e.g. after POSTing a message so we don't wait
  // for the idle poll to notice the new generation).
  kick() {
    if (this.stopped) return;
    clearTimeout(this.timer);
    if (this.es) this.es.close();
    this.cleanClose = false;
    this.connect();
  }

  close() {
    this.stopped = true;
    clearTimeout(this.timer);
    if (this.es) this.es.close();
    this.es = null;
  }
}

// Shared EventSource wrapper that JSON-decodes every message: used by the
// global and title streams, which rely on EventSource's built-in
// auto-reconnect and need no resume parameters.
function jsonEventSource(path, onEvent) {
  const es = new EventSource(streamUrl(path));
  es.onmessage = (e) => {
    let ev;
    try {
      ev = JSON.parse(e.data);
    } catch {
      return;
    }
    onEvent(ev);
  };
  return es;
}

// Global (all-chats) lifecycle stream: generation_started / done /
// chat_updated for EVERY chat, so the sidebar tracks background generations
// (breathing titles) without a per-chat stream each.
// The server sends a generating_snapshot first on every (re)connect, so the
// browser's built-in EventSource auto-reconnect is self-healing — no manual
// retry logic, no polling.
export class GlobalStream {
  constructor(onEvent) {
    this.es = jsonEventSource("/api/stream", onEvent);
  }

  close() {
    this.es?.close();
    this.es = null;
  }
}

// Title stream: the title task's DEDICATED channel. Carries only
// {chat_id, title} events (an auto-generated title became final), fully
// independent from the engine streams so generation traffic can never delay
// or drop a title update. Like GlobalStream, it relies on EventSource
// auto-reconnect; a missed event only delays a sidebar label until the next
// chat-list load (title events are idempotent: chat id + the title itself).
export class TitleStream {
  constructor(onEvent) {
    this.es = jsonEventSource("/api/stream/titles", onEvent);
  }

  close() {
    this.es?.close();
    this.es = null;
  }
}
