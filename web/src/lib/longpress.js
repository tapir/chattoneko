// Long press → the app's own action sheet, on touch only.
//
// Spread the returned handlers on the element that should react (an image
// thumbnail, a file chip). They are deliberately NOT on message text: a long
// press there is how a phone selects and copies words, and the browser keeps
// that. Desktop is left alone too — a mouse never starts the timer and
// right-click keeps the native menu (open image in a new tab, save, …).

const HOLD_MS = 450; // Android's own long-press threshold; the timer is the iOS fallback
const SLOP_PX = 10; // finger drift tolerated before the press is a scroll

export function longPress(onfire) {
  let timer = null;
  let el = null;
  let x = 0;
  let y = 0;
  let touch = false; // a touch press is down (what contextmenu checks)
  let fired = false;

  function fire() {
    if (fired) return;
    fired = true;
    clearTimeout(timer);
    // The browser still delivers a click after a long press: swallow it, or
    // the sheet opens the lightbox underneath itself.
    swallowClick(el);
    onfire();
  }

  function end() {
    clearTimeout(timer);
    timer = null;
    touch = false;
    fired = false;
  }

  return {
    onpointerdown(e) {
      if (e.pointerType === 'mouse' || !e.isPrimary) return;
      el = e.currentTarget;
      x = e.clientX;
      y = e.clientY;
      touch = true;
      fired = false;
      clearTimeout(timer);
      timer = setTimeout(fire, HOLD_MS);
    },
    onpointermove(e) {
      if (touch && Math.hypot(e.clientX - x, e.clientY - y) > SLOP_PX) end();
    },
    onpointerup: end,
    // The browser reclaims the gesture once the list starts scrolling.
    onpointercancel: end,
    oncontextmenu(e) {
      if (!touch) return; // a real right-click: the native menu stays
      e.preventDefault(); // …and the long-press one ("download image") goes
      fire(); // Android's signal, when it beats the timer
    },
  };
}

function swallowClick(el) {
  if (!el) return;
  const stop = (e) => {
    e.preventDefault();
    e.stopPropagation();
  };
  el.addEventListener('click', stop, { capture: true, once: true });
  // Some browsers send no click at all — don't eat the next real tap.
  setTimeout(() => el.removeEventListener('click', stop, { capture: true }), 700);
}
