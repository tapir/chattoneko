// LIFO registry of currently-open overlays (sheets, popovers, dialogs).
// The native Android back-button handler closes the most recently opened
// overlay before falling back to history navigation / app exit.

const stack = [];

// Register the callback that closes an overlay; returns an unregister
// function suitable as a $effect / onDestroy cleanup.
export function registerOverlay(close) {
  stack.push(close);
  return () => {
    const i = stack.lastIndexOf(close);
    if (i >= 0) stack.splice(i, 1);
  };
}

// Close the most recently opened overlay. Returns true if one was closed.
export function closeTopOverlay() {
  const close = stack.pop();
  if (!close) return false;
  close();
  return true;
}
