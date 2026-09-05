// Pointer-drag resizing for user-resizable panels (sidebar, tools sheet,
// system sheet). Widths persist to localStorage.

export function loadPanelWidth(key, fallback, { min, max }) {
  try {
    const v = parseInt(localStorage.getItem(key) ?? "", 10);
    if (Number.isFinite(v)) return Math.min(max, Math.max(min, v));
  } catch {
    /* private mode */
  }
  return fallback;
}

export function savePanelWidth(key, w) {
  try {
    localStorage.setItem(key, String(w));
  } catch {
    /* private mode */
  }
}

// Start a pointer-drag resize. `start` = current width, `invert` = true when
// dragging the LEFT edge of a right-anchored panel (moving left grows it).
export function startPanelResize(e, { start, min, max, invert = false, onChange, onEnd }) {
  e.preventDefault();
  e.stopPropagation();
  const startX = e.clientX;
  const clamp = (w) => Math.round(Math.min(max, Math.max(min, w)));
  function onMove(ev) {
    const dx = ev.clientX - startX;
    onChange(clamp(start + (invert ? -dx : dx)));
  }
  function onUp() {
    window.removeEventListener("pointermove", onMove);
    window.removeEventListener("pointerup", onUp);
    onEnd?.();
  }
  window.addEventListener("pointermove", onMove);
  window.addEventListener("pointerup", onUp);
}
