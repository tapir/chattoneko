import { streamUrl } from "./server.js";

// The app's ONE SSE connection per tab.
//
// Browsers cap an origin at 6 concurrent HTTP/1.1 connections. This used to
// be three persistent streams per tab (per-chat deltas, all-chats lifecycle,
// titles), so two open tabs saturated the pool and every fetch from the
// second tab — including the send that starts a generation — queued inside
// the browser until the first tab happened to free a socket.
//
// One multiplexed connection instead: /api/stream always carries the
// lifecycle + title events of every chat, and ?chat=<id>&after=<seq> adds the
// open chat's delta half. That half implements the B4 resume contract:
// events are deduped by seq, reconnects pass ?after=<lastSeq>, and the stream
// reconnects on a slow poll after clean closes (idle/done).

const RETRY_MS = 1000; // unexpected drop
const POLL_MS = 4000; // clean close (server sent idle/done)
// The server pings every 20s. EventSource only fires onerror on a CLOSED
// socket; a half-open one (phone changed networks, NAT mapping expired) looks
// open forever while the reply and its `done` land in a dead pipe and the UI
// spins until a reload. Nothing at all for 3 pings' worth → reconnect.
const STALL_MS = 60_000;

// Types that only ever arrive on the global half (no per-chat copy), so they
// route to the sidebar handler even when they name the open chat.
const GLOBAL_ONLY = new Set(["generating_snapshot", "config_changed", "title"]);

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

export class AppStream {
  // chatId: the open chat whose delta half this connection also carries, or
  // null on the home view (global half only). onChat receives the open
  // chat's own events, onGlobal everything else (sidebar/titles/config).
  constructor(chatId, onChat, onGlobal) {
    this.chatId = chatId ?? null;
    this.onChat = onChat;
    this.onGlobal = onGlobal;
    this.lastSeq = -1;
    this.epoch = null;
    this.es = null;
    this.stopped = false;
    this.cleanClose = false;
    this.timer = null;
    this.connect();
  }

  url() {
    const params = {};
    if (this.chatId) {
      params.chat = this.chatId;
      params.after = this.lastSeq;
    }
    return streamUrl("/api/stream", params);
  }

  connect() {
    if (this.stopped) return;
    const es = openStream(
      this.url(),
      (ev) => {
        const own = !!this.chatId && ev.chat_id === this.chatId;
        if (own) {
          // Seq and epoch belong to the SUBSCRIBED chat's space. The global
          // half also carries other chats' lifecycle events, stamped with
          // THEIR hub's seq/epoch — feeding those to the dedupe below would
          // poison the baseline and silently drop the open chat's deltas.
          //
          // The server's seq space resets when the chat hub is pruned and
          // recreated (e.g. after a disconnect on an idle chat); every event
          // carries the hub epoch so we can detect that and reset the dedupe
          // baseline — with a stale lastSeq the entire next generation
          // (deltas, done) would be silently dropped.
          if (ev.epoch && ev.epoch !== this.epoch) {
            this.epoch = ev.epoch;
            this.lastSeq = -1;
          }
          // The open chat's lifecycle events (generation_started / done /
          // chat_updated) reach us on BOTH halves — the engine fans every
          // chat event out to global subscribers too. Both copies carry the
          // same seq, so this dedupe drops the second one.
          if (typeof ev.seq === "number") {
            if (ev.seq <= this.lastSeq) return;
            this.lastSeq = ev.seq;
          }
          if (ev.type === "done" || ev.type === "idle") this.cleanClose = true;
        }
        if (own && !GLOBAL_ONLY.has(ev.type)) this.onChat(ev);
        else this.onGlobal(ev);
      },
      () => this.kick(),
    );
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
