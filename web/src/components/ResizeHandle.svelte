<script>
  // Edge drag handle for user-resizable panels. `invert` for handles on the
  // LEFT edge of a right-anchored panel (dragging left grows the panel).
  import { loadPanelWidth, savePanelWidth, startPanelResize } from '../lib/resize.js';

  let {
    storageKey, // localStorage key for persistence
    fallback = 448,
    min = 320,
    max = 960,
    invert = false,
    label = 'Resize panel',
    width = $bindable(fallback),
  } = $props();

  // Closure: reads the (static) config props once at init without capturing
  // them as reactive dependencies.
  width = (() => loadPanelWidth(storageKey, fallback, { min, max }))();

  function start(e) {
    startPanelResize(e, {
      start: width,
      min,
      max,
      invert,
      onChange: (w) => (width = w),
      onEnd: () => savePanelWidth(storageKey, width),
    });
  }
</script>

<div
  class={[
    'absolute top-0 z-10 h-full w-1.5 hover:bg-accent/50',
    invert ? 'left-0 cursor-ew-resize' : 'right-0 cursor-col-resize'
  ]}
  role="separator"
  aria-orientation="vertical"
  aria-label={label}
  title="Drag to resize"
  onpointerdown={start}
></div>
