// Tiny theme manager: follows the system dark/light preference by default,
// persists to localStorage only once the user sets a theme explicitly.
// Until then, live system changes are followed too.

const KEY = 'chattoneko-theme';

const media =
  typeof window !== 'undefined' && typeof window.matchMedia === 'function'
    ? window.matchMedia('(prefers-color-scheme: light)')
    : null;

function systemTheme() {
  return media?.matches ? 'light' : 'dark';
}

// Reactive current theme — updated by applyTheme. Import `themeState` and read
// `.current` inside $derived/$effect for live theme tracking (e.g. Sonner).
export const themeState = $state({ current: 'dark' });

function read() {
  try {
    const t = localStorage.getItem(KEY);
    return t === 'light' || t === 'dark' ? t : null;
  } catch {
    return null;
  }
}

function getTheme() {
  return read() ?? systemTheme();
}

function applyTheme(theme) {
  const dark = theme === 'dark';
  themeState.current = theme;
  document.documentElement.classList.toggle('dark', dark);
  document.documentElement.style.colorScheme = dark ? 'dark' : 'light';
  syncSystemBars(dark);
}

// Native (Capacitor) only: keep the Android status/navigation bar icon
// brightness (and, pre-Android 15, the bar background color) in sync with
// the app theme. Style.Dark = icons for a dark background (light icons).
// Dynamic import so the web build never loads the plugin; failures are
// swallowed (non-native webviews, missing plugin).
function syncSystemBars(dark) {
  if (typeof window === 'undefined' || !window.Capacitor?.isNativePlatform?.()) return;
  import('@capacitor/status-bar')
    .then(({ StatusBar, Style }) => {
      StatusBar.setStyle({ style: dark ? Style.Dark : Style.Light }).catch(() => {});
      StatusBar.setBackgroundColor({ color: dark ? '#171717' : '#fafafa' }).catch(() => {});
    })
    .catch(() => {});
}

export function setTheme(theme) {
  applyTheme(theme);
  try {
    localStorage.setItem(KEY, theme);
  } catch {
    /* private mode */
  }
}

export function toggleTheme() {
  setTheme(getTheme() === 'dark' ? 'light' : 'dark');
}

let listening = false;

/** Apply the stored/system theme. Call once on app mount (head script already
    seeded it pre-paint, this just re-syncs + enables reactive state). Also
    starts following live system theme changes until the user sets an explicit
    preference. */
export function initTheme() {
  applyTheme(getTheme());
  if (media && !listening) {
    listening = true;
    media.addEventListener('change', () => {
      if (read() === null) applyTheme(systemTheme());
    });
  }
}
