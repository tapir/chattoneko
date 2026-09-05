// Central app state (Svelte 5 runes in .svelte.js). Owns auth, config,
// sidebar chat list, the active chat, live generation state and toasts.

import { SvelteSet } from "svelte/reactivity";
import {
  api,
  setUnauthorizedHandler,
  normalizeChat,
  normalizeMessage,
} from "./api.js";
import { ChatStream, GlobalStream, TitleStream } from "./stream.svelte.js";
import {
  isNative,
  getServerUrl,
  setServerUrl,
  getToken,
  setToken,
  isTokenExpired,
} from "./server.js";
import { Typewriter } from "./typewriter.js";
import { toast as sonnerToast } from "svelte-sonner";

const CHAT_PAGE = 30;

// Client-side staging rules for attachments, mirroring the server's
// (internal/attach + internal/api limits, exposed via /api/config). Rejects
// surface at attach time; the server re-validates at send time.
const IMAGE_EXTS = ["jpg", "jpeg", "png", "gif", "webp"];
const TEXT_EXTS = [
  "txt", "md", "markdown", "yaml", "yml", "json", "jsonl", "ndjson",
  "xml", "csv", "tsv", "toml", "ini", "conf", "log", "go", "py", "js",
  "ts", "c", "cpp", "h", "rs", "java", "sh", "sql", "html", "css",
];
// Single source of truth for the attachable extensions: the composer's file
// input accept="" attribute is derived from these two lists.
export const ACCEPT_FILE_EXTS = [...IMAGE_EXTS, ...TEXT_EXTS];
// Fallbacks if /api/config limits haven't loaded yet.
const FALLBACK_MAX_FILES = 8; // internal/api maxUploadFiles
const FALLBACK_MAX_RAW_IMAGE_BYTES = 64 * 1024 * 1024; // attach.MaxRawUploadBytes

let pendingSeq = 0;
const pendingId = () => `pending-${Date.now()}-${++pendingSeq}`;

function placeholderAssistant(chatId, messageId) {
  return {
    id: messageId,
    chat_id: chatId,
    role: "assistant",
    status: "generating",
    content: "",
    reasoning: "",
    error: "",
    tool_call_id: "",
    name: "",
    tool_calls: [],
    attachments: [],
    seq: Number.MAX_SAFE_INTEGER,
    created_at: Date.now(),
    updated_at: Date.now(),
  };
}

class AppState {
  // auth
  authChecked = $state(false);
  authEnabled = $state(false);
  authed = $state(false);
  username = $state("");
  serverDown = $state(false);
  // Native (Capacitor) build with no configured server yet: the setup
  // screen collects the server address before any API call happens.
  needsServerSetup = $state(false);
  nativeApp = $state(false);
  serverUrl = $state(""); // configured server address (native only)

  // Server setup status: does the provider + designated models exist yet?
  // Sourced from /api/meta `setup_complete`. null = not known yet.
  setupComplete = $state(null);
  // Settings overlay open state. When setupComplete === false the overlay is
  // forced open and cannot be dismissed (see SettingsSheet.svelte).
  settingsOpen = $state(false);

  // server config (models whitelist, tools catalog, limits)
  config = $state(null);
  // Effective system prompt for the ACTIVE chat (config prompt + enabled tool
  // definitions), served by GET /chats/:id; falls back to the config-level one.
  systemPrompt = $state("");

  // sidebar
  chats = $state([]);
  chatsHasMore = $state(false);
  chatsLoading = $state(false);

  // active chat
  activeChatId = $state(null);
  chat = $state(null);
  messages = $state([]);
  chatLoading = $state(false);
  generating = $state(false);
  // The message currently being sent, until the server has it. MessageList
  // renders it at the tail of the list (bubble + staged previews + scramble)
  // so it shows the instant you hit send instead of after the create-chat,
  // upload and send round trips. Deliberately NOT part of `messages`: nothing
  // that reloads the list (refreshChat, the `idle` event a fresh stream
  // subscribe emits) can wipe it, and no id reconciliation is needed — the
  // real rows arrive through the normal paths and this simply goes away.
  outgoing = $state(null);
  // server attachment id -> local object URL of the file just uploaded under
  // it, so the real message's <img> can keep painting the local copy until the
  // server's is fully decoded (AttachmentImage does the flip and the revoke).
  // Plain Map, not $state: read once at mount, never rendered from.
  previews = new Map();
  previewFor(id) {
    return this.previews.get(id);
  }
  forgetPreview(id) {
    this.previews.delete(id);
  }
  // Chats with an in-flight generation (for the sidebar breathing title, #3).
  // SvelteSet: add/delete must be reactive — a plain Set in $state is not
  // tracked, so background-chat breathing titles silently never updated.
  // Entries for the active chat come from its per-chat SSE stream;
  // background chats (no stream attached) are driven by the global stream —
  // see handleGlobalEvent/reconcileGeneratingFlags.
  chatGeneratingIds = new SvelteSet();
  // live generation: rebuilt purely from stream events (B4 contract)
  live = $state(null); // {messageId, display, reasoningDisplay, toolCalls[], status, error}

  // draft model + reasoning effort for a not-yet-created chat
  newChatModel = $state("");
  newChatEffort = $state("");

  // sidebar search (#4): query + results (empty query => show recent chats)
  searchQuery = $state("");
  searchResults = $state(null); // null = not searching; [] = searching, no results
  searchLoading = $state(false);

  // top-bar stats (#6): per-chat token totals + context window for the active model
  chatUsage = $state(null); // {prompt_tokens, completion_tokens}
  modelInfo = $state([]); // [{id, context_window}] from /api/config

  // Per-chat tool toggles: sparse map of overrides on top of the configured
  // defaults (tool_defaults, else the catalog's default_enabled). Persisted
  // with the chat (chats.tools_json) and re-hydrated on open, so they survive
  // switching chats and reloads; a NEW chat starts empty (= defaults).
  chatToolOverrides = $state({}); // {toolName: bool} sparse map

  // Pending attachments, keyed by chat id ("" = not-yet-created draft chat).
  // Lives in the store (not the Composer) because ensureChat() remounts the
  // Composer on first send; a component-local list would be wiped. Entries
  // are staged client-side only (File + object-URL preview): nothing touches
  // the server until send(), so attaching never creates a chat or stores
  // orphan blobs (File instances pass through $state unwrapped — Svelte only
  // proxies plain objects/arrays).
  pendingAttachments = $state({});

  // Composer drafts, keyed the same way. Component-local drafts were lost
  // when ensureChat() remounted the Composer mid-send (the failure path
  // restored text into a destroyed instance), and on every chat switch.
  drafts = $state({});

  // non-reactive internals
  stream = null;
  tw = null; // content typewriter
  twr = null; // reasoning typewriter
  liveContent = "";
  liveReasoning = "";
  gstream = null; // global (all-chats) lifecycle stream
  tstream = null; // title task's dedicated stream

  constructor() {
    setUnauthorizedHandler(() => {
      if (this.authEnabled && this.authed) {
        // The streams were opened with the now-dead token baked into their
        // URLs and cannot re-authenticate — close them so they stop
        // replaying/retrying forever on the login screen.
        this.closeAllStreams();
        setToken(""); // the rejected token is dead — drop it
        this.authed = false;
        this.toast("error", "Session expired — please log in again");
      }
    });
  }

  // Closes every SSE stream (per-chat, global, titles). login() re-attaches
  // fresh ones built with the new token.
  closeAllStreams() {
    this.detachStream();
    if (this.gstream) {
      this.gstream.close();
      this.gstream = null;
    }
    if (this.tstream) {
      this.tstream.close();
      this.tstream = null;
    }
  }

  // ---- boot / auth ----

  async init() {
    this.nativeApp = isNative();
    this.serverUrl = getServerUrl();
    if (this.nativeApp && !getServerUrl()) {
      this.needsServerSetup = true;
      this.authChecked = true;
      return;
    }
    try {
      const meta = await api.meta();
      this.authEnabled = !!meta?.auth_enabled;
      this.setupComplete = !!meta?.setup_complete;
      if (this.authEnabled) {
        const token = getToken();
        if (token && isTokenExpired(token)) {
          // The 90-day token ran out: skip the doomed /me call and drop
          // straight to the login screen (same on web and mobile).
          setToken("");
          this.authed = false;
        } else {
          try {
            const me = await api.me();
            this.authed = true;
            this.username = me?.username ?? "";
          } catch {
            setToken("");
            this.authed = false;
          }
        }
      } else {
        this.authed = true;
      }
    } catch {
      this.serverDown = true;
    }
    this.authChecked = true;
    if (this.authed) {
      this.attachGlobalStream();
      this.attachTitleStream();
      await Promise.all([this.loadConfig(), this.loadChats()]);
    }
  }

  // Called by the login screen after a successful /api/meta probe on a
  // server without auth: persist the server URL and re-run the boot flow.
  async setServer(url) {
    setServerUrl(url);
    this.serverUrl = url;
    this.needsServerSetup = false;
    this.serverDown = false;
    this.authChecked = false;
    await this.init();
  }

  // Called by the login screen when a probed server reports auth enabled:
  // persist the confirmed address (and mark auth as on) but stay on the same
  // screen so the credential fields can appear below the locked address.
  confirmServer(url) {
    setServerUrl(url);
    this.serverUrl = url;
    this.authEnabled = true;
    this.serverDown = false;
  }

  // Re-opened from the sidebar over an existing server: show the setup
  // screen WITHOUT clearing anything; the X there calls cancelChangeServer.
  changeServer() {
    this.needsServerSetup = true;
  }

  // Dismiss a deliberate server-switch screen; nothing was cleared yet.
  cancelChangeServer() {
    this.needsServerSetup = false;
  }

  async login(username, password) {
    // One path for web and native: the server verifies the credentials
    // against the env-var-configured username/password (CHATTO_USERNAME /
    // CHATTO_PASSWORD) and returns a signed JWT (90-day TTL), stored and
    // sent as a Bearer token everywhere.
    const res = await api.login(username, password); // throws on failure
    setToken(res?.token ?? "");
    this.authed = true;
    this.needsServerSetup = false; // mobile: leave the setup screen behind
    this.username = username;
    this.attachGlobalStream();
    this.attachTitleStream();
    await Promise.all([this.loadConfig(), this.loadChats()]);
  }

  async logout() {
    // JWTs are stateless: there is nothing to invalidate server-side —
    // discarding the token IS the logout (same on web and native).
    this.closeAllStreams();
    setToken("");
    location.reload();
  }

  async loadConfig() {
    try {
      this.config = await api.config();
      if (!this.newChatModel)
        this.newChatModel = this.config?.models?.default_chat_model ?? "";
      // model_info: [{id, context_window}] for the whitelist (#6 top-bar context %)
      this.modelInfo = Array.isArray(this.config?.model_info)
        ? this.config.model_info
        : [];
    } catch (e) {
      this.toast("error", `Failed to load config: ${e.message}`);
    }
  }

  // After the setup/config overlay saves: refresh the setup-complete flag
  // from /api/meta and reload the chat-facing config (whitelist, model_info,
  // tools) so the main UI reflects the changes without a page reload.
  async refreshAfterSetup() {
    try {
      const meta = await api.meta();
      this.setupComplete = !!meta?.setup_complete;
      // Auth is env-var driven and can't change here, but keep the gate in
      // sync with the server so a 401 (no valid token) correctly drops to
      // the login screen instead of a broken authenticated-but-tokenless state.
      this.authEnabled = !!meta?.auth_enabled;
    } catch {
      /* keep the previous values */
    }
    await this.loadConfig();
  }

  // Context window (tokens) for a model id; 0 = unknown.
  contextWindowFor(modelId) {
    if (!modelId) return 0;
    const info = this.modelInfo.find((m) => m.id === modelId);
    return info?.context_window ?? 0;
  }

  // Reasoning-effort levels a model accepts (from the provider's /models
  // endpoint, served via /api/config model_info). Empty = not applicable.
  effortOptionsFor(modelId) {
    if (!modelId) return [];
    const opts = this.modelInfo.find((m) => m.id === modelId)?.reasoning_efforts;
    return Array.isArray(opts) ? opts : [];
  }

  // Effort to preselect in the composer: "medium" when supported, else the
  // provider's default for the model, else the middle of the list.
  defaultEffortFor(modelId) {
    const opts = this.effortOptionsFor(modelId);
    if (opts.length === 0) return "";
    if (opts.includes("medium")) return "medium";
    const d = this.modelInfo.find((m) => m.id === modelId)?.reasoning_default;
    if (d && opts.includes(d)) return d;
    return opts[Math.floor(opts.length / 2)] ?? opts[0];
  }

  // ---- toasts ----

  toast(kind, text) {
    if (kind === "error") sonnerToast.error(text);
    else sonnerToast(text);
  }

  // ---- sidebar chat list ----

  async loadChats() {
    this.chatsLoading = true;
    try {
      const page = await api.listChats({ limit: CHAT_PAGE });
      this.chats = page;
      this.chatsHasMore = page.length >= CHAT_PAGE;
      this.reconcileGeneratingFlags(page);
    } catch (e) {
      this.toast("error", `Failed to load chats: ${e.message}`);
    } finally {
      this.chatsLoading = false;
    }
  }

  async loadMoreChats() {
    if (this.chatsLoading || !this.chatsHasMore || this.chats.length === 0)
      return;
    const last = this.chats[this.chats.length - 1];
    this.chatsLoading = true;
    try {
      const page = await api.listChats({
        limit: CHAT_PAGE,
        before: last.updated_at,
        beforeId: last.id,
      });
      const seen = new Set(this.chats.map((c) => c.id));
      const fresh = page.filter((c) => !seen.has(c.id));
      this.chats = [...this.chats, ...fresh];
      this.chatsHasMore = page.length >= CHAT_PAGE;
      this.reconcileGeneratingFlags(page);
    } catch (e) {
      this.toast("error", `Failed to load chats: ${e.message}`);
    } finally {
      this.chatsLoading = false;
    }
  }

  async deleteChat(id) {
    try {
      await api.deleteChat(id);
    } catch (e) {
      this.toast("error", `Failed to delete chat: ${e.message}`);
      return;
    }
    this.chats = this.chats.filter((c) => c.id !== id);
    if (this.activeChatId === id) {
      this.closeChat();
      location.hash = "#/";
    }
  }

  // ---- global stream: sidebar state for background chats (#3) ----

  // Only the ACTIVE chat has a per-chat SSE stream; the global stream
  // (attachGlobalStream) pushes lifecycle events for every chat so the
  // sidebar can breathe/unbreathe and pick up renames without it.
  attachGlobalStream() {
    if (this.gstream) return;
    this.gstream = new GlobalStream((ev) => this.handleGlobalEvent(ev));
  }

  // ---- title stream: auto-generated titles pushed by the title task ----

  // The title task's dedicated stream carries ONLY title events, so sidebar
  // titles update live without being queued behind generation traffic.
  attachTitleStream() {
    if (this.tstream) return;
    this.tstream = new TitleStream((ev) => this.handleTitleEvent(ev));
  }

  handleTitleEvent(ev) {
    if (!ev.chat_id || ev.title == null) return;
    // Sidebar entry.
    const c = this.chats.find((c) => c.id === ev.chat_id);
    if (c) c.title = ev.title;
    // Active chat (header, document title).
    if (this.chat && this.chat.id === ev.chat_id) {
      this.chat = { ...this.chat, title: ev.title };
    }
  }

  handleGlobalEvent(ev) {
    // The active chat's own stream owns its state; global events for it are
    // duplicates (applying them is idempotent but unnecessary).
    switch (ev.type) {
      case "generating_snapshot": {
        // Full server-side truth on (re)connect: additions and removals are
        // both safe. The active chat is excluded — its stream owns it.
        const live = new Set(ev.chat_ids ?? []);
        for (const id of live) {
          if (id !== this.activeChatId) this.chatGeneratingIds.add(id);
        }
        for (const id of [...this.chatGeneratingIds]) {
          if (id !== this.activeChatId && !live.has(id)) {
            this.chatGeneratingIds.delete(id);
          }
        }
        break;
      }
      case "generation_started": {
        if (ev.chat_id && ev.chat_id !== this.activeChatId) {
          this.chatGeneratingIds.add(ev.chat_id);
          this.bumpChatInList(ev.chat_id);
        }
        break;
      }
      case "done": {
        if (!ev.chat_id || ev.chat_id === this.activeChatId) break;
        this.chatGeneratingIds.delete(ev.chat_id);
        this.bumpChatInList(ev.chat_id);
        break;
      }
      case "chat_updated": {
        if (!ev.chat_id || ev.title == null) break;
        const c = this.chats.find((c) => c.id === ev.chat_id);
        if (c) c.title = ev.title;
        break;
      }
      case "config_changed": {
        // The MCP tool catalog was rebuilt server-side after a settings
        // save (async reconnect); refetch the chat-facing config so the
        // tools menu and model picker update without a page reload.
        this.loadConfig();
        break;
      }
    }
  }

  // Sidebar reconciliation from the chat list payload (initial load, focus
  // refresh): each chat carries a live `generating` flag. The global stream
  // keeps it current afterwards.
  reconcileGeneratingFlags(page) {
    for (const c of page) {
      if (c.id === this.activeChatId) continue; // stream owns the active chat
      if (c.generating) this.chatGeneratingIds.add(c.id);
      else this.chatGeneratingIds.delete(c.id);
    }
  }

  // ---- per-chat tool toggles ----

  // Effective enabled state for one catalog tool: the chat's override wins,
  // otherwise the server-config default_enabled.
  toolEnabled(tool) {
    const o = this.chatToolOverrides;
    return tool.name in o ? !!o[tool.name] : !!tool.default_enabled;
  }

  toggleTool(name, checked) {
    this.chatToolOverrides = { ...this.chatToolOverrides, [name]: checked };
    if (this.chat) {
      this.patchChat({ tools: { ...this.chatToolOverrides } });
    }
  }

  // ---- active chat ----

  attachStream(chatId) {
    this.detachStream();
    this.stream = new ChatStream(chatId, (ev) => this.handleStreamEvent(ev));
  }

  detachStream() {
    if (this.stream) {
      this.stream.close();
      this.stream = null;
    }
  }

  async openChat(id) {
    if (this.activeChatId === id && this.chat) return;
    this.detachStream();
    this.destroyLive();
    // Handoff: if the chat we're leaving is mid-generation, its entry in
    // chatGeneratingIds must survive the switch (its stream — which owned
    // that entry — is gone now; the global stream clears it on done).
    if (this.generating && this.activeChatId) {
      this.chatGeneratingIds.add(this.activeChatId);
    }
    this.generating = false;
    this.activeChatId = id;
    this.chat = null;
    this.messages = [];
    this.chatUsage = null;
    // Cleared while loading; the chat's persisted toggles replace these below.
    this.chatToolOverrides = {};
    this.chatLoading = true;
    try {
      const { chat, messages, usage, systemPrompt } = await api.getChat(id);
      if (this.activeChatId !== id) return; // navigated away meanwhile
      this.chat = chat;
      this.messages = messages;
      this.chatUsage = usage ?? null; // per-chat token totals for the top bar
      if (systemPrompt != null) this.systemPrompt = systemPrompt;
      this.chatToolOverrides = { ...(chat.tools ?? {}) };
      const gen = messages.find(
        (m) => m.role === "assistant" && m.status === "generating",
      );
      if (gen) {
        this.generating = true;
        this.startLive(gen.id);
      }
      this.attachStream(id);
    } catch (e) {
      this.toast("error", `Failed to open chat: ${e.message}`);
      if (this.activeChatId === id) {
        this.activeChatId = null;
        location.hash = "#/";
      }
    } finally {
      this.chatLoading = false;
    }
  }

  closeChat() {
    this.detachStream();
    this.destroyLive();
    if (this.generating && this.activeChatId) {
      this.chatGeneratingIds.add(this.activeChatId);
    }
    this.generating = false;
    this.activeChatId = null;
    this.chat = null;
    this.messages = [];
    this.chatUsage = null;
    this.systemPrompt = "";
    this.chatToolOverrides = {};
  }

  async refreshChat() {
    const id = this.activeChatId;
    if (!id) return;
    try {
      const { chat, messages, usage, systemPrompt } = await api.getChat(id);
      if (this.activeChatId !== id) return;
      this.chat = chat;
      this.messages = messages;
      if (usage) this.chatUsage = usage;
      if (systemPrompt != null) this.systemPrompt = systemPrompt;
      // Keep the sidebar entry in sync with the freshly fetched title.
      const c = this.chats.find((c) => c.id === id);
      if (c && chat.title && c.title !== chat.title) c.title = chat.title;
      const still = messages.find(
        (m) => m.role === "assistant" && m.status === "generating",
      );
      if (!still) this.generating = false;
    } catch (e) {
      this.toast("error", `Failed to refresh chat: ${e.message}`);
    }
  }

  // ---- sidebar search (#4) ----

  // Run a title search; empty query clears back to recent chats.
  async runSearch(query) {
    this.searchQuery = query;
    const q = query.trim();
    if (!q) {
      this.searchResults = null;
      this.searchLoading = false;
      return;
    }
    this.searchLoading = true;
    try {
      this.searchResults = await api.searchChats(q);
    } catch (e) {
      this.toast("error", `Search failed: ${e.message}`);
      this.searchResults = [];
    } finally {
      this.searchLoading = false;
    }
  }

  clearSearch() {
    this.searchQuery = "";
    this.searchResults = null;
  }

  onFocus() {
    if (!this.authed) return;
    this.loadChats();
    if (this.activeChatId && !this.generating) this.refreshChat();
  }

  // ---- sending ----

  // Ensure a server-side chat exists before the first completion request
  // (crash-safe ordering: POST /chats first, then the message). Returns
  // {chatId, created}: created marks a chat made by THIS call so send() can
  // roll it back if the send fails (no empty chat left in the sidebar).
  async ensureChat() {
    if (this.activeChatId && this.chat)
      return { chatId: this.activeChatId, created: false };
    const model = this.newChatModel || undefined;
    // Carry the composer's effort selection (explicit choice, else the
    // displayed default) so the first generation already honors it.
    const effort = this.newChatEffort || this.defaultEffortFor(model);
    const params = {};
    if (effort) params.reasoning_effort = effort;
    // Tool overrides toggled before the chat existed.
    const tools = Object.keys(this.chatToolOverrides).length
      ? { ...this.chatToolOverrides }
      : undefined;
    const chat = await api.createChat(
      model,
      Object.keys(params).length ? params : undefined,
      tools,
    );
    this.chats.unshift(chat);
    this.chat = chat;
    this.messages = [];
    this.activeChatId = chat.id;
    this.generating = false;
    this.attachStream(chat.id);
    location.hash = `#/c/${chat.id}`;
    return { chatId: chat.id, created: true };
  }

  // attachments: staged pending entries ({file, previewUrl, ...}); they
  // reach the server only now, at send time.
  async send(content, attachments = []) {
    // Staged entries already carry the attachment shape the bubble renders
    // (id/filename/kind) plus previewUrl, a local object URL for images that
    // the gallery/viewer prefer over the server URL when present.
    this.outgoing = {
      id: "outgoing",
      role: "user",
      status: "outgoing",
      content,
      attachments,
    };
    try {
      const { chatId, created } = await this.ensureChat();
      try {
        let ids = [];
        if (attachments.length > 0) {
          const metas = await api.uploadAttachments(
            chatId,
            attachments.map((a) => a.file),
          );
          ids = metas.map((m) => m.id);
          // Same order in and out (the server rejects the whole request on
          // any bad file, never skips one). Populated BEFORE the message is
          // sent, so it's there however the real row arrives (POST response
          // or the SSE broadcast). ponytail: a failed send leaves its ids
          // here — orphan uuids that never render, not worth a cleanup path.
          metas.forEach((m, i) => {
            if (attachments[i]?.previewUrl)
              this.previews.set(m.id, attachments[i].previewUrl);
          });
        }
        const res = await api.sendMessage(chatId, content, ids);
        const um = res?.user_message ?? res?.message;
        if (um && !this.messages.some((m) => m.id === um.id)) {
          this.messages.push(normalizeMessage(um));
        }
        const aid = res?.assistant_message_id ?? res?.assistant_id;
        if (aid) {
          this.generating = true;
          // The SSE generation_started (+ first deltas) can arrive BEFORE this
          // POST resolves; restarting live here would wipe those deltas and
          // the beginning of the reply would be missing until refresh.
          if (!(this.live && this.live.messageId === aid)) this.startLive(aid);
          if (!this.messages.some((m) => m.id === aid)) {
            this.messages.push(placeholderAssistant(chatId, aid));
          }
        } else {
          this.generating = true;
        }
        this.stream?.kick();
        this.bumpChatInList(chatId);
        // Preview object URLs are NOT revoked here: the real row's <img> is
        // still painting them (see `previews`); AttachmentImage revokes each
        // once the server copy has taken over.
      } catch (e) {
        // A chat created by this attempt must not linger messageless (it
        // would sit in the sidebar with nothing in it); the delete also
        // cascades away any orphans a failed upload left behind.
        if (created) await this.deleteChat(chatId);
        throw e;
      }
    } catch (e) {
      this.toast("error", `Failed to send: ${e.message}`);
      throw e;
    } finally {
      // Success: the real user message + assistant placeholder were pushed
      // above in this same tick, so the swap renders in one frame. Failure:
      // the Composer restores the draft and staged files.
      this.outgoing = null;
    }
  }

  bumpChatInList(chatId) {
    const idx = this.chats.findIndex((c) => c.id === chatId);
    if (idx > 0) {
      const [c] = this.chats.splice(idx, 1);
      c.updated_at = Date.now();
      this.chats.unshift(c);
    }
  }

  async regenerate() {
    const id = this.activeChatId;
    if (!id || this.generating) return;
    try {
      await api.regenerate(id);
      this.generating = true;
      await this.refreshChat();
      this.stream?.kick();
    } catch (e) {
      this.generating = false;
      this.toast("error", `Failed to regenerate: ${e.message}`);
    }
  }

  async editMessage(messageId, content, attachmentIds) {
    const id = this.activeChatId;
    if (!id) return;
    try {
      await api.editMessage(id, messageId, content, attachmentIds);
      this.generating = true;
      await this.refreshChat();
      this.stream?.kick();
    } catch (e) {
      this.generating = false;
      this.toast("error", `Failed to edit message: ${e.message}`);
    }
  }

  async stopGeneration() {
    const id = this.activeChatId;
    if (!id) return;
    try {
      await api.stopGeneration(id);
    } catch (e) {
      if (e.status === 404) {
        // generation already finished between click and request
        this.generating = false;
        await this.refreshChat();
      } else {
        this.toast("error", `Failed to stop: ${e.message}`);
      }
    }
  }

  // ---- chat settings ----

  async patchChat(patch) {
    const id = this.activeChatId;
    if (!id || !this.chat) return;
    const prev = this.chat;
    this.chat = normalizeChat({ ...this.chat, ...patch });
    try {
      await api.patchChat(id, patch);
    } catch (e) {
      this.chat = prev;
      this.toast("error", `Failed to save settings: ${e.message}`);
    }
  }

  // ---- composer draft + attachments ----

  // Draft text for the current composer target ("" = not-yet-created chat).
  draftFor() {
    return this.drafts[this.activeChatId ?? ""] ?? "";
  }

  setDraft(text) {
    const key = this.activeChatId ?? "";
    this.drafts = { ...this.drafts, [key]: text };
  }

  // Pending attachments for the current composer target. ensureChat() may
  // remount the Composer (route change), so this lives in the store.
  pendingList() {
    return this.pendingAttachments[this.activeChatId ?? ""] ?? [];
  }

  // Stage files for the next send entirely client-side: no chat is created
  // and nothing is uploaded until send(). Validation mirrors the server's
  // rules so rejects surface immediately at attach time.
  addAttachments(files) {
    const key = this.activeChatId ?? "";
    const list = this.pendingAttachments[key] ?? [];
    const limits = this.config?.limits ?? {};
    const maxFiles = limits.max_upload_files ?? FALLBACK_MAX_FILES;
    const maxTextBytes = limits.upload_max_file_bytes ?? 0;
    const maxRawImageBytes =
      limits.max_raw_upload_bytes ?? FALLBACK_MAX_RAW_IMAGE_BYTES;

    const staged = [];
    for (const file of files) {
      if (list.length + staged.length >= maxFiles) {
        this.toast("error", `Too many attachments (max ${maxFiles})`);
        break;
      }
      const name = file.name || "file";
      const ext = name.includes(".")
        ? name.slice(name.lastIndexOf(".") + 1).toLowerCase()
        : "";
      const isImage = IMAGE_EXTS.includes(ext);
      if (!isImage && !TEXT_EXTS.includes(ext)) {
        this.toast("error", `${name}: unsupported file type`);
        continue;
      }
      if (file.size === 0) {
        this.toast("error", `${name}: empty file`);
        continue;
      }
      // Images are downscaled into the stored cap server-side, so the raw
      // bytes cap is the meaningful client check for them; text files hit
      // the stored cap directly.
      const tooLarge = isImage
        ? file.size > maxRawImageBytes
        : maxTextBytes > 0 && file.size > maxTextBytes;
      if (tooLarge) {
        this.toast("error", `${name}: file too large`);
        continue;
      }
      staged.push({
        id: pendingId(), // local only; the server assigns real ids at send
        file,
        filename: name,
        size: file.size,
        kind: isImage ? "image" : "text",
        previewUrl: isImage ? URL.createObjectURL(file) : "",
      });
    }
    if (staged.length) {
      this.pendingAttachments = {
        ...this.pendingAttachments,
        [key]: [...list, ...staged],
      };
    }
    return staged;
  }

  // Explicit removal revokes the preview object URL; clearPendingAttachments
  // (send-time) does NOT — the failure path restores the same entries.
  removePendingAttachment(attachmentId) {
    const key = this.activeChatId ?? "";
    const all = this.pendingAttachments[key] ?? [];
    const att = all.find((a) => a.id === attachmentId);
    if (att?.previewUrl) URL.revokeObjectURL(att.previewUrl);
    this.pendingAttachments = {
      ...this.pendingAttachments,
      [key]: all.filter((a) => a.id !== attachmentId),
    };
  }

  restorePendingAttachment(att) {
    const key = this.activeChatId ?? "";
    const list = this.pendingAttachments[key] ?? [];
    if (!list.some((a) => a.id === att.id)) {
      this.pendingAttachments = {
        ...this.pendingAttachments,
        [key]: [...list, att],
      };
    }
  }

  clearPendingAttachments() {
    const key = this.activeChatId ?? "";
    if (this.pendingAttachments[key]) {
      this.pendingAttachments = { ...this.pendingAttachments, [key]: [] };
    }
  }

  // ---- live generation state ----

  startLive(messageId) {
    this.destroyLive();
    this.liveContent = "";
    this.liveReasoning = "";
    this.live = {
      messageId,
      display: "",
      reasoningDisplay: "",
      toolCalls: [],
      attachments: [],
      status: "generating",
      error: "",
    };
    this.tw = new Typewriter((d) => {
      this.liveContent = d;
      if (this.live) this.live.display = d;
    });
    this.twr = new Typewriter((d) => {
      this.liveReasoning = d;
      if (this.live) this.live.reasoningDisplay = d;
    });
  }

  destroyLive() {
    this.tw?.destroy();
    this.twr?.destroy();
    this.tw = null;
    this.twr = null;
    this.live = null;
    this.liveContent = "";
    this.liveReasoning = "";
  }

  upsertToolCall(callId, patch) {
    if (!this.live) return;
    const existing = this.live.toolCalls.find((t) => t.call_id === callId);
    if (existing) {
      Object.assign(existing, patch);
    } else {
      this.live.toolCalls.push({
        call_id: callId,
        name: "",
        args: "",
        result: "",
        is_error: false,
        pending: true,
        ...patch,
      });
    }
  }

  handleStreamEvent(ev) {
    switch (ev.type) {
      case "idle": {
        // No active generation: reconcile in case we missed the tail of a
        // previous generation (reconnect after the grace period). Reload
        // persisted state and drop any stuck live/generating UI. `idle` only
        // fires once per subscribe (the server keeps the SSE open), so this
        // doesn't loop.
        this.generating = false;
        if (this.activeChatId) this.chatGeneratingIds.delete(this.activeChatId);
        this.destroyLive();
        this.refreshChat();
        break;
      }
      case "generation_started": {
        this.generating = true;
        if (this.activeChatId) this.chatGeneratingIds.add(this.activeChatId);
        if (!(this.live && this.live.messageId === ev.message_id))
          this.startLive(ev.message_id);
        if (
          ev.message_id &&
          !this.messages.some((m) => m.id === ev.message_id)
        ) {
          this.messages.push(
            placeholderAssistant(this.activeChatId, ev.message_id),
          );
        }
        break;
      }
      case "delta":
        if (this.live && ev.message_id === this.live.messageId)
          this.tw?.push(ev.content ?? "");
        break;
      case "reasoning_delta":
        if (this.live && ev.message_id === this.live.messageId)
          this.twr?.push(ev.content ?? "");
        break;
      case "tool_call_started":
        this.upsertToolCall(ev.call_id, { name: ev.name ?? "", pending: true });
        break;
      case "tool_call_delta": {
        // Incremental arguments fragment: append live so the call's arguments
        // stream in. tool_call_done re-delivers the full arguments, which
        // self-heals any reconnect mid-stream (partial args replaced).
        if (!this.live) break;
        const t = this.live.toolCalls.find((t) => t.call_id === ev.call_id);
        if (t) t.args += ev.arguments ?? "";
        else this.upsertToolCall(ev.call_id, { args: ev.arguments ?? "", pending: true });
        break;
      }
      case "tool_call_done": {
        // The start event can be missed (name streamed late); the done
        // event always carries the name, so set it here too.
        const patch = { args: ev.arguments ?? "", pending: true };
        if (ev.name) patch.name = ev.name;
        this.upsertToolCall(ev.call_id, patch);
        break;
      }
      case "tool_result": {
        const patch = {
          result: ev.result ?? "",
          is_error: !!ev.is_error,
          pending: false,
        };
        // Never wipe a known name with undefined (omitempty on the wire).
        if (ev.name) patch.name = ev.name;
        this.upsertToolCall(ev.call_id, patch);
        break;
      }
      case "attachment_created": {
        // A tool persisted a file onto the generating assistant message
        // (e.g. create_text_file, show_image) — show the attachment chip
        // once the reply lands.
        const att = ev.attachment;
        if (
          att &&
          this.live &&
          ev.message_id === this.live.messageId &&
          !this.live.attachments.some((a) => a.id === att.id)
        ) {
          this.live.attachments.push(att);
        }
        break;
      }
      case "status":
        if (
          this.live &&
          (!ev.message_id || ev.message_id === this.live.messageId)
        ) {
          this.live.status = ev.status ?? this.live.status;
          this.live.error = ev.error ?? "";
        }
        break;
      case "done": {
        this.tw?.flush();
        this.twr?.flush();
        const mid = this.live?.messageId;
        if (mid) {
          const idx = this.messages.findIndex((m) => m.id === mid);
          const finalStatus =
            this.live?.status && this.live.status !== "generating"
              ? this.live.status
              : "complete";
          const finalCalls = (this.live?.toolCalls ?? []).map((t) => ({
            provider_call_id: t.call_id,
            name: t.name,
            arguments: t.args,
          }));
          if (idx >= 0) {
            this.messages[idx] = {
              ...this.messages[idx],
              status: finalStatus,
              content: this.liveContent,
              reasoning: this.liveReasoning,
              error: this.live?.error ?? "",
              tool_calls: finalCalls,
              // Keep tool-created files visible until refreshChat lands.
              attachments: [...(this.live?.attachments ?? [])],
            };
          }
        }
        this.destroyLive();
        this.generating = false;
        if (this.activeChatId) this.chatGeneratingIds.delete(this.activeChatId);
        // done carries per-turn usage and is replayed, so applying it here
        // can't be lost between reconnects.
        if (ev.prompt_tokens || ev.completion_tokens || ev.duration_ms) {
          const cur = this.chatUsage ?? {
            prompt_tokens: 0,
            completion_tokens: 0,
          };
          this.chatUsage = {
            prompt_tokens: cur.prompt_tokens + (ev.prompt_tokens ?? 0),
            completion_tokens:
              cur.completion_tokens + (ev.completion_tokens ?? 0),
          };
          // Stamp the assistant message with its per-turn stats (#5).
          if (mid) {
            const idx = this.messages.findIndex((m) => m.id === mid);
            if (idx >= 0) {
              this.messages[idx] = {
                ...this.messages[idx],
                prompt_tokens: ev.prompt_tokens ?? 0,
                completion_tokens: ev.completion_tokens ?? 0,
                duration_ms: ev.duration_ms ?? 0,
              };
            }
          }
        }
        // Sync persisted state (real seq ordering, tool result messages).
        this.refreshChat();
        break;
      }
      case "user_message": {
        const m = ev.message;
        if (m && !this.messages.some((x) => x.id === m.id)) {
          this.messages.push(normalizeMessage(m));
        }
        break;
      }
      case "chat_updated": {
        if (this.chat && ev.title != null)
          this.chat = { ...this.chat, title: ev.title };
        const c = this.chats.find((c) => c.id === this.activeChatId);
        if (c && ev.title != null) c.title = ev.title;
        break;
      }
      case "settings_updated": {
        if (ev.chat) {
          this.chat = normalizeChat(ev.chat);
          // Changed on another client: the toggles render from the override
          // map, so follow the server's copy.
          this.chatToolOverrides = { ...(this.chat.tools ?? {}) };
        }
        break;
      }
      case "messages_reset": {
        // History was truncated on another client (edit/resend or regenerate).
        this.refreshChat();
        break;
      }
    }
  }
}

export const app = new AppState();
