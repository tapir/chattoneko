// Shared display formatters (token counts, durations, context windows).
// Single source of truth so the header stats and the per-message popover
// always render numbers the same way.

// Compact token count: 950 -> "950", 1200 -> "1.2k", 2_500_000 -> "2.5M".
export function formatTokens(n) {
  if (!n || n <= 0) return "0";
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1000) return `${(n / 1000).toFixed(1)}k`;
  return String(n);
}

// Same compaction but em-dash for "no data" (per-message stats where a
// missing value means "not recorded", not zero).
export function formatTokensOrDash(n) {
  return !n || n <= 0 ? "—" : formatTokens(n);
}

// File size: 940 -> "940 B", 2048 -> "2.0 KB", 5_400_000 -> "5.4 MB".
export function formatBytes(n) {
  if (n == null || n < 0) return "";
  if (n < 1024) return `${Math.round(n)} B`;
  const units = ["KB", "MB", "GB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v >= 10 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
}

// Generation duration: 420 -> "420 ms", 2300 -> "2.3 s", 95000 -> "1m 35s".
export function formatDuration(ms) {
  if (!ms || ms <= 0) return "—";
  if (ms < 1000) return `${ms} ms`;
  const s = ms / 1000;
  return s < 60
    ? `${s.toFixed(1)} s`
    : `${Math.floor(s / 60)}m ${Math.round(s % 60)}s`;
}
