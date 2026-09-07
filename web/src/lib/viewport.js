// Keyboard-aware shell height.
//
// The soft keyboard shrinks the viewport in ONE jump — the native WebView is
// resized outright, mobile browsers do it via interactive-widget=resizes-content
// (index.html) — so the whole shell snapped upward instantly. `100dvh` cannot
// be transitioned (viewport units resolve at used-value time, so no computed
// value ever changes), so the height is mirrored into a px var on <html> for
// .h-app / .min-h-app to read and CSS animates that instead.
//
// ponytail: no visualViewport.offsetTop compensation — a platform that PANS
// for the keyboard (iOS Safari) instead of resizing can leave the shell above
// the visible rectangle. Add it if iOS web shows a gap.

const KEYBOARD_JUMP = 120; // px — a keyboard is 250+, browser chrome is ~60

let lastW = window.innerWidth;
let lastH = window.innerHeight;

// Height the shell should have. Normally window.innerHeight, but a platform
// that shrinks only the VISUAL viewport for the keyboard (an edge-to-edge
// WebView that pans instead of resizing, iOS Safari) needs the smaller of the
// two. Pinch zoom shrinks the visual viewport too and must be ignored, or
// zooming would cut the shell down to the zoomed rectangle.
function layoutHeight() {
  const vv = window.visualViewport;
  if (!vv || Math.abs(vv.scale - 1) > 0.01) return window.innerHeight;
  return Math.min(window.innerHeight, Math.round(vv.height));
}

function measure() {
  const w = window.innerWidth;
  const h = layoutHeight();
  const delta = Math.abs(h - lastH);
  // Only a keyboard-shaped jump (big height delta, same width) animates:
  // rotations and browser chrome showing/hiding must track instantly or the
  // shell visibly lags behind them. A zero delta is the SECOND event of the
  // same burst (window resize and visualViewport resize both fire) and must
  // leave the class alone — toggling it off there clears the transition
  // before the first style recalc and the shell snaps instead of gliding.
  if (delta > 0) {
    document.documentElement.classList.toggle(
      'kb-anim',
      w === lastW && delta > KEYBOARD_JUMP
    );
  }
  document.documentElement.style.setProperty('--app-h', `${h}px`);
  lastW = w;
  lastH = h;
}

export function initViewport() {
  measure();
  window.addEventListener('resize', measure);
  window.visualViewport?.addEventListener('resize', measure);
}
