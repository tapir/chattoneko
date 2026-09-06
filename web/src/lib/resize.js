// Pointer-drag resizing for user-resizable panels (sidebar, tools sheet,
// system sheet). Widths persist to localStorage.

import { lsGet, lsSet } from "./persist.js";

export function loadPanelWidth(key, fallback, { min, max }) {
  const v = parseInt(lsGet(key), 10);
  return Number.isFinite(v) ? Math.min(max, Math.max(min, v)) : fallback;
}

export function savePanelWidth(key, w) {
  lsSet(key, w);
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
