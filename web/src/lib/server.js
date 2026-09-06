// Runtime server configuration for the Capacitor (mobile) build + JWT token
// storage for both builds. The web build served by the Go backend never
// touches the server URL: with no stored URL every request stays
// same-origin ("/api"). On native, the user provides the server address
// once (login screen); the URL and the JWT persist in localStorage under
// the WebView's origin.

import { lsGet, lsSet } from "./persist.js";

const URL_KEY = "chattoneko-server-url";
const TOKEN_KEY = "chattoneko-token";

// Capacitor injects window.Capacitor into the WebView — no npm dependency
// needed for detection.
export function isNative() {
  return !!(
    typeof window !== "undefined" && window.Capacitor?.isNativePlatform?.()
  );
}

export function getServerUrl() {
  return lsGet(URL_KEY);
}

export function getToken() {
  return lsGet(TOKEN_KEY);
}

// normalizeServerUrl trims input, adds a default http:// scheme, and drops
// trailing slashes / path junk. Returns "" for invalid input.
export function normalizeServerUrl(raw) {
  let u = (raw || "").trim();
  if (!u) return "";
  if (!/^https?:\/\//i.test(u)) u = "http://" + u;
  try {
    const parsed = new URL(u);
    if (parsed.protocol !== "http:" && parsed.protocol !== "https:") return "";
    const path = parsed.pathname.replace(/\/+$/, "");
    return parsed.origin + path;
  } catch {
    return "";
  }
}

export function setServerUrl(url) {
  lsSet(URL_KEY, url);
}

export function setToken(token) {
  lsSet(TOKEN_KEY, token);
}

// isTokenExpired decodes the JWT payload (no signature check — the server
// does that) and reports whether exp is in the past. Unparseable tokens
// count as expired so the UI falls back to the login screen instead of
// looping on 401s.
export function isTokenExpired(token) {
  try {
    const payload = token.split(".")[1];
    const claims = JSON.parse(
      atob(payload.replace(/-/g, "+").replace(/_/g, "/")),
    );
    return typeof claims.exp !== "number" || claims.exp * 1000 <= Date.now();
  } catch {
    return true;
  }
}

// streamUrl builds an EventSource URL (absolute when a server is
// configured), appending ?token= when a token is set — EventSource can't
// send headers, and GET requests accept the token as a query parameter.
export function streamUrl(path, params = {}) {
  const q = new URLSearchParams(params);
  const token = getToken();
  if (token) q.set("token", token);
  const qs = q.toString();
  return `${getServerUrl()}${path}${qs ? "?" + qs : ""}`;
}
