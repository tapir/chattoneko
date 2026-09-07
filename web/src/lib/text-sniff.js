// The client's "is this a text file" rule: the same one the server applies in
// attach.looksText (no NUL, decodes as UTF-8, >=95% printable/whitespace), so
// attachments are filtered by content instead of by an extension list — .go,
// .toml, .log, extensionless dumps and anything else textual all just work.
// Sampling the head is enough to catch binaries here; the server re-checks the
// whole file at send time. Rune-free so test-smoke.mjs can import it directly.

const SNIFF_BYTES = 64 * 1024;

export async function looksText(file) {
  if (file.size === 0) return true; // the caller reports the empty-file case
  const head = new Uint8Array(await file.slice(0, SNIFF_BYTES).arrayBuffer());
  if (head.includes(0)) return false;
  // Non-fatal decoding maps invalid UTF-8 (and a tail cut mid-rune) to U+FFFD,
  // which then fails the printable ratio the way the server's utf8.Valid does.
  const chars = [...new TextDecoder().decode(head)];
  const good = chars.filter(
    (c) =>
      c === "\t" || c === "\n" || c === "\r" ||
      (c >= " " && c !== "\u007f" && c !== "\ufffd"),
  ).length;
  return chars.length > 0 && good / chars.length >= 0.95;
}
