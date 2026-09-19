// localStorage in one place. Every read falls back, every write is swallowed:
// Safari/Firefox private mode and sandboxed webviews throw on access, and none
// of what lives here (theme, panel widths, server URL, JWT) is worth crashing
// over. An empty value removes the key.

export function lsGet(key, fallback = "") {
  try {
    return localStorage.getItem(key) ?? fallback;
  } catch {
    return fallback;
  }
}

export function lsSet(key, value) {
  try {
    if (value == null || value === "") localStorage.removeItem(key);
    else localStorage.setItem(key, String(value));
  } catch {
    /* private mode */
  }
}
