import { streamUrl } from "./server.js";

// SSE stream manager for one chat. Implements the B4 resume contract:
// events are deduped by seq, reconnects pass ?after=<lastSeq>, and the
// stream reconnects on a slow poll after clean closes (idle/done) so
// generations started from other clients are picked up within seconds.

const RETRY_MS = 1000; // unexpected drop
const POLL_MS = 4000; // clean close (server sent idle/done)
// The server pings every 20s. EventSource only fires onerror on a CLOSED
// socket; a half-open one (phone changed networks, NAT mapping expired) looks
// open forever while the reply and its `done` land in a dead pipe and the UI
// spins until a reload. Nothing at all for 3 pings' worth → reconnect.
const STALL_MS = 60_000;

// One EventSource: JSON-decodes messages, swallows pings, and after STALL_MS
// of silence closes itself and calls onStall so the owner can reconnect. The
// returned es.close() also disarms the watchdog.
function openStream(url, onEvent, onStall) {
  const es = new EventSource(url);
  let wd;
  const arm = () => {
    clearTimeout(wd);
    wd = setTimeout(() => {
      es.close();
      onStall();
    }, STALL_MS);
  };
  const close = es.close.bind(es);
  es.close = () => {
    clearTimeout(wd);
    close();
  };
  es.onmessage = (e) => {
    arm();
    let ev;
    try {
      ev = JSON.parse(e.data);
    } catch {
      return;
    }
    if (ev.type !== "ping") onEvent(ev);
  };
  arm();
  return es;
}

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
    const es = openStream(this.url(), (ev) => {
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
    }, () => this.kick());
    this.es = es;
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

// Global and title streams need no resume parameters: they rely on
// EventSource's built-in auto-reconnect for closed sockets and on the stall
// watchdog for half-open ones (a stall just opens a fresh EventSource).
class SimpleStream {
  constructor(path, onEvent) {
    this.path = path;
    this.onEvent = onEvent;
    this.connect();
  }

  connect() {
    this.es = openStream(streamUrl(this.path), this.onEvent, () => this.connect());
  }

  close() {
    this.es?.close();
    this.es = null;
  }
}

// Global (all-chats) lifecycle stream: generation_started / done /
// chat_updated for EVERY chat, so the sidebar tracks background generations
// (breathing titles) without a per-chat stream each. The server sends a
// generating_snapshot first on every (re)connect, so reconnects self-heal.
export class GlobalStream extends SimpleStream {
  constructor(onEvent) {
    super("/api/stream", onEvent);
  }
}

// Title stream: the title task's DEDICATED channel. Carries only
// {chat_id, title} events (an auto-generated title became final), fully
// independent from the engine streams so generation traffic can never delay
// or drop a title update. A missed event only delays a sidebar label until
// the next chat-list load (title events are idempotent: chat id + title).
export class TitleStream extends SimpleStream {
  constructor(onEvent) {
    super("/api/stream/titles", onEvent);
  }
}
