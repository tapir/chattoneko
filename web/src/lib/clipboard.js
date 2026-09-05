// Clipboard helper. Prefers the async Clipboard API, but that only exists in
// secure contexts (HTTPS/localhost) — over plain HTTP (e.g. http://host:8080)
// navigator.clipboard is undefined, and writeText can also reject (permission
// denied, focus). Fall back to the legacy textarea+execCommand path in those
// cases so copy works everywhere. Resolves false instead of throwing only if
// both paths fail.

export async function copyText(text) {
  if (!text) return false;
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // fall through to the legacy path
    }
  }
  return legacyCopy(text);
}

function legacyCopy(text) {
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.setAttribute('readonly', '');
  ta.style.position = 'fixed';
  ta.style.top = '0';
  ta.style.left = '-9999px';
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try {
    ok = document.execCommand('copy');
  } catch {
    ok = false;
  }
  ta.remove();
  return ok;
}
