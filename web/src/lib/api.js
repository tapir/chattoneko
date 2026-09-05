// REST client for the chattoneko backend + response normalizers.
// Centralizes JSON shapes so backend contract drift is a one-file fix.

import { getServerUrl, getToken } from "./server.js";

// Base URL: same-origin "/api" for the web build; the user-configured
// server on the Capacitor (mobile) build.
function base() {
  const u = getServerUrl();
  return u ? u + "/api" : "/api";
}

// fetch defaults: the JWT travels as a Bearer token on every request (web
// and native alike) — there are no cookies or credentials anymore.
function baseInit(init = {}) {
  const headers = { ...(init.headers || {}) };
  const token = getToken();
  if (token) headers["Authorization"] = `Bearer ${token}`;
  const out = { ...init };
  if (Object.keys(headers).length) out.headers = headers;
  return out;
}

export class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

// Set by app state; called on any 401 so the UI can drop to the login screen.
let onUnauthorized = () => {};
export function setUnauthorizedHandler(fn) {
  onUnauthorized = fn;
}

async function request(method, path, body) {
  const init = { method };
  if (body !== undefined) {
    init.headers = { "Content-Type": "application/json" };
    init.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(base() + path, baseInit(init));
  } catch {
    throw new ApiError(0, "Cannot reach server");
  }
  if (res.status === 401) onUnauthorized();
  if (res.status === 204) return null;
  const text = await res.text();
  let data = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = null;
    }
  }
  if (!res.ok) {
    throw new ApiError(
      res.status,
      (data && data.error) || res.statusText || `HTTP ${res.status}`,
    );
  }
  return data;
}

function parseMaybeJson(v, fallback) {
  if (v == null) return fallback;
  if (typeof v === "string") {
    try {
      return JSON.parse(v);
    } catch {
      return fallback;
    }
  }
  return v;
}

// Chat: tolerate both parsed params/tools and *_json string columns.
export function normalizeChat(c) {
  if (!c) return null;
  const params = parseMaybeJson(c.params ?? c.params_json, {});
  const tools = parseMaybeJson(c.tools ?? c.tools_json, {});
  return { ...c, params, tools, title: c.title ?? "", model: c.model ?? "" };
}

export function normalizeMessage(m) {
  return {
    tool_calls: [],
    attachments: [],
    content: "",
    reasoning: "",
    error: "",
    tool_call_id: "",
    name: "",
    status: "complete",
    ...m,
  };
}

export const api = {
  meta: () => request("GET", "/meta"),
  me: () => request("GET", "/auth/me"),
  login: (username, password) =>
    request("POST", "/auth/login", { username, password }),
  config: () => request("GET", "/config"),

  // Server settings (initial setup + admin edits). GET returns the full
  // config, secrets included (api_key, MCP header values), so the settings
  // UI can display and edit them in place; PUT applies a partial patch; setupModels
  // fetches per-model metadata from the provider's /models for the given ids.
  // provider = { baseUrl, apiKey } carries unsaved form values; the server
  // falls back to the stored provider config for any field left empty.
  setup: () => request("GET", "/setup"),
  saveSetup: (patch) => request("PUT", "/setup", patch),
  setupModels: (modelIds, provider = {}) =>
    request("POST", "/setup/models", {
      model_ids: modelIds,
      base_url: provider.baseUrl ?? "",
      api_key: provider.apiKey ?? "",
    }),

  listChats: async ({ limit = 30, before, beforeId } = {}) => {
    const q = new URLSearchParams({ limit: String(limit) });
    if (before != null) q.set("before", String(before));
    if (beforeId) q.set("before_id", beforeId);
    const data = await request("GET", `/chats?${q}`);
    const arr = Array.isArray(data) ? data : (data?.chats ?? []);
    return arr.map(normalizeChat);
  },
  createChat: async (model, params, tools) => {
    const body = {};
    if (model) body.model = model;
    if (params) body.params = params;
    if (tools) body.tools = tools;
    const data = await request("POST", "/chats", body);
    return normalizeChat(data?.chat ?? data);
  },
  getChat: async (id) => {
    const data = await request("GET", `/chats/${id}`);
    const chat = normalizeChat(data?.chat ?? data);
    const messages = (data?.messages ?? data?.chat?.messages ?? []).map(
      normalizeMessage,
    );
    const usage = data?.usage ?? null;
    // Effective system prompt for this chat (config prompt + tool defs).
    const systemPrompt = data?.system_prompt ?? null;
    return { chat, messages, usage, systemPrompt };
  },
  // Full conversation log (plain text), shown in the Logs panel (#6).
  chatLog: async (id) => {
    let res;
    try {
      res = await fetch(`${base()}/chats/${id}/log`, baseInit());
    } catch {
      throw new ApiError(0, "Cannot reach server");
    }
    if (res.status === 401) onUnauthorized();
    const text = await res.text();
    if (!res.ok) throw new ApiError(res.status, res.statusText || `HTTP ${res.status}`);
    return text;
  },
  // Search chats by title (#4). Returns [] for empty query.
  searchChats: async (q) => {
    const data = await request("GET", `/chats?q=${encodeURIComponent(q)}`);
    const arr = Array.isArray(data) ? data : (data?.chats ?? []);
    return arr.map(normalizeChat);
  },
  patchChat: async (id, patch) => {
    const data = await request("PATCH", `/chats/${id}`, patch);
    return normalizeChat(data?.chat ?? data);
  },
  deleteChat: (id) => request("DELETE", `/chats/${id}`),

  sendMessage: (id, content, attachmentIds) =>
    request("POST", `/chats/${id}/messages`, {
      content,
      attachment_ids: attachmentIds,
    }),
  editMessage: (id, mid, content, attachmentIds) =>
    request("PATCH", `/chats/${id}/messages/${mid}`, {
      content,
      ...(attachmentIds ? { attachment_ids: attachmentIds } : {}),
    }),
  regenerate: (id) => request("POST", `/chats/${id}/regenerate`),
  stopGeneration: (id) => request("DELETE", `/chats/${id}/generation`),

  uploadAttachments: async (id, files) => {
    const fd = new FormData();
    for (const f of files) fd.append("files", f, f.name);
    let res;
    try {
      res = await fetch(`${base()}/chats/${id}/attachments`, baseInit({
        method: "POST",
        body: fd,
      }));
    } catch {
      throw new ApiError(0, "Cannot reach server");
    }
    if (res.status === 401) onUnauthorized();
    let data = null;
    try {
      data = await res.json();
    } catch {
      data = null;
    }
    if (!res.ok)
      throw new ApiError(
        res.status,
        data?.error || res.statusText || `HTTP ${res.status}`,
      );
    return Array.isArray(data) ? data : (data?.attachments ?? []);
  },
  // <img>/<a> targets can't carry the Authorization header; GET requests
  // accept the token as ?token= instead.
  attachmentUrl: (id) => {
    const url = `${base()}/attachments/${id}`;
    const token = getToken();
    return token ? `${url}?token=${encodeURIComponent(token)}` : url;
  },
  // Plain-text body of a text attachment (the viewer overlay). The server
  // serves text attachments as text/plain regardless of their detected mime,
  // so the body is always safe to render as inert text.
  attachmentText: async (id) => {
    let res;
    try {
      res = await fetch(`${base()}/attachments/${id}`, baseInit());
    } catch {
      throw new ApiError(0, "Cannot reach server");
    }
    if (res.status === 401) onUnauthorized();
    const text = await res.text();
    if (!res.ok) {
      let message = res.statusText || `HTTP ${res.status}`;
      try {
        message = JSON.parse(text)?.error || message;
      } catch {
        /* non-JSON error body */
      }
      throw new ApiError(res.status, message);
    }
    return text;
  },
  // Vision-model description of an image attachment (the text the chat model
  // is shown in place of the image). 404 when the attachment was never
  // described.
  attachmentDescription: async (id) => {
    let res;
    try {
      res = await fetch(`${base()}/attachments/${id}/description`, baseInit());
    } catch {
      throw new ApiError(0, "Cannot reach server");
    }
    if (res.status === 401) onUnauthorized();
    const text = await res.text();
    if (!res.ok) throw new ApiError(res.status, res.statusText || `HTTP ${res.status}`);
    return text;
  },

  // Probe a candidate server URL (native setup flow): GET /api/meta without
  // any auth. 5s timeout — the browser default on unreachable hosts can wait
  // ~30s+. Throws ApiError(0) when unreachable.
  probeServer: async (url) => {
    let res;
    try {
      res = await fetch(`${url}/api/meta`, { signal: AbortSignal.timeout(5000) });
    } catch {
      throw new ApiError(0, "Cannot reach server");
    }
    if (!res.ok) throw new ApiError(res.status, res.statusText || `HTTP ${res.status}`);
    return res.json();
  },

};
