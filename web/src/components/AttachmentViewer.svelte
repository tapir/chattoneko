<script>
  // Fullscreen attachment lightbox. Text attachments render as inert
  // monospace; images get a pan/zoom stage. Opened by clicking any
  // attachment in a message (viewer.open() in lib/viewer.svelte.js) —
  // App.svelte mounts this once at the root.
  //
  // A native <dialog> (the same primitive Confirm uses) puts the
  // overlay in the browser TOP LAYER: it escapes every stacking context,
  // traps focus, and closes on Escape for free — no z-index needed.
  //
  // Colouring splits by pane. The IMAGE viewer is black in both themes — the
  // deliberate departure from the semantic-token rule that makes pictures pop
  // — so its chrome is raw white-on-black. The TEXT pane wears the app's own
  // tokens and follows the theme: `.dark` sits on <html> and the top layer
  // changes painting, not DOM ancestry, so tokens and `dark:` variants
  // resolve inside the dialog exactly as they do everywhere else.
  import { onMount, onDestroy } from 'svelte';
  import { api } from '../lib/api.js';
  import { app } from '../lib/state.svelte.js';
  import { formatBytes } from '../lib/format.js';
  import { isNative } from '../lib/server.js';
  import { registerOverlay } from '../lib/overlays.svelte.js';
  import { Check, ChevronLeft, ChevronRight, Copy, Download, Share2, X, ZoomIn, ZoomOut } from '@lucide/svelte';
  import { copyText } from '../lib/clipboard.js';

  let {
    attachment,
    items = null, // optional gallery set from viewer.open(att, items)
    onclose,
    onprev = null,
    onnext = null,
  } = $props();

  let isImage = $derived(attachment?.kind === 'image');
  // <img>/<a> can't carry the Authorization header; attachmentUrl appends
  // ?token= (the server accepts a token on GETs). Keeping the real URL
  // visible to the browser also keeps download trivial.
  let url = $derived(
    attachment ? attachment.previewUrl || api.attachmentUrl(attachment.id) : ''
  );

  // ---- gallery navigation ----
  // `items` is the message's image set. With more than one member the viewer
  // gains prev/next (swipe, arrow keys, side chevrons) plus a position
  // counter; a text file or a lone image opens exactly as it always did.
  let navIndex = $derived(
    Array.isArray(items) ? items.findIndex((a) => a.id === attachment?.id) : -1,
  );
  let hasNav = $derived(navIndex >= 0 && items.length > 1);
  let navLabel = $derived(hasNav ? `${navIndex + 1} / ${items.length}` : '');
  // Subtitle: counter · size, either half dropped when it doesn't apply.
  let subtitle = $derived(
    [navLabel, attachment?.size ? formatBytes(attachment.size) : '']
      .filter(Boolean)
      .join(' · '),
  );

  // Chrome: light-on-dark over the black image viewer, theme tokens over the
  // text pane (which flips with the app theme by itself).
  const BTN_BASE =
    'flex size-9 shrink-0 items-center justify-center rounded-full transition-colors';
  let BTN = $derived(
    `${BTN_BASE} ${
      isImage
        ? 'text-white/70 hover:bg-white/15 hover:text-white focus-visible:bg-white/15 focus-visible:text-white'
        : 'text-muted-foreground hover:bg-accent hover:text-accent-foreground focus-visible:bg-accent focus-visible:text-accent-foreground'
    }`,
  );
  // App-only: the Android WebView has no navigator.share, and a browser has
  // its own save/share affordances.
  const native = isNative();
  // Gallery chevrons: hover-capable pointers only — touch gets the swipe
  // gesture, and floating arrows would eat a narrow screen.
  const NAV_BTN =
    'absolute top-1/2 z-10 hidden size-11 -translate-y-1/2 items-center justify-center rounded-full bg-black/30 text-white/70 backdrop-blur transition-colors hover:bg-black/50 hover:text-white focus-visible:bg-black/50 focus-visible:text-white [@media(pointer:fine)]:flex';

  let dlg = $state();
  let unregisterBack;

  onMount(() => {
    dlg?.showModal();
    // Android back closes the viewer instead of navigating away under it.
    unregisterBack = registerOverlay(() => close());
  });
  onDestroy(() => unregisterBack?.());

  // Every dismissal path — Escape, the ✕ button, a backdrop tap, Android
  // back — funnels through the dialog's native close event.
  function closed() {
    onclose?.();
  }

  // Arrow keys walk the gallery (Escape already closes it, natively).
  function onKeydown(e) {
    if (!hasNav) return;
    if (e.key === 'ArrowLeft') {
      e.preventDefault();
      onprev?.();
    } else if (e.key === 'ArrowRight') {
      e.preventDefault();
      onnext?.();
    }
  }

  // Exit is class-driven: .closing plays the fade-out, then the native
  // close() unmounts. A fixed timeout beats animationend here — animationend
  // bubbles, so any finite child animation would dismiss early.
  const REDUCED = matchMedia('(prefers-reduced-motion: reduce)').matches;
  let closing = $state(false);
  function close() {
    if (closing || !dlg?.open) return;
    closing = true; // class attr below drives the exit animation
    setTimeout(() => dlg.close(), REDUCED ? 0 : 150);
  }

  // ---- text pane ----
  let text = $state(null);
  let textError = $state('');
  let loadingText = $state(false);
  // Guards a slow response from overwriting a newer one (the overlay is
  // remounted per attachment, but a retry can still race).
  let textSeq = 0;

  $effect(() => {
    const id = attachment?.id;
    if (!id || isImage) return;
    loadText(id);
  });

  // Copy is the one affordance a text viewer wants beyond download — these
  // files are usually code. (CopyButton can't be reused: it is themed.)
  let copied = $state(false);
  let copyTimer = null;
  async function copyContents() {
    copied = await copyText(text);
    clearTimeout(copyTimer);
    copyTimer = setTimeout(() => (copied = false), 1200);
  }
  onDestroy(() => clearTimeout(copyTimer));

  // ---- share (native only) ----
  // Capacitor's Share plugin wants a local file:// URL, so the bytes are
  // staged in the app cache dir first — already exposed to other apps by the
  // FileProvider in mobile/android/.../res/xml/file_paths.xml. `url` is the
  // viewer's own source, so a staged, not-yet-uploaded image shares from its
  // local object URL too.
  let sharing = $state(false);
  async function share() {
    if (sharing) return;
    sharing = true;
    try {
      // no-store: the <img> in this very dialog caches the same URL without
      // CORS headers, and a poisoned cache entry makes the fetch fail ("Failed
      // to fetch") on pictures that are visibly on screen. Servers now send
      // Vary: Origin, which fixes it for good — this keeps sharing working
      // against older ones too.
      const res = await fetch(url, { cache: 'no-store' });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const { Filesystem, Directory } = await import('@capacitor/filesystem');
      const { Share } = await import('@capacitor/share');
      // The extension is what gives the share sheet its MIME type; stripping
      // separators keeps a hostile filename inside the cache dir.
      const name = (attachment?.filename || 'attachment').replace(/[^\w.-]+/g, '_');
      await Filesystem.writeFile({
        path: name,
        data: await blobBase64(await res.blob()),
        directory: Directory.Cache,
        recursive: true, // the plugin's cache folder may not exist yet
      });
      const { uri } = await Filesystem.getUri({ path: name, directory: Directory.Cache });
      await Share.share({ files: [uri] });
    } catch (e) {
      // Dismissing the system sheet comes back as a rejection, not a failure.
      if (!/cancel/i.test(e?.message ?? ''))
        app.toast('error', `Couldn't share: ${e?.message ?? 'unknown error'}`);
    } finally {
      sharing = false;
    }
  }

  // Filesystem's binary write path takes base64 (no `encoding`); FileReader is
  // the only portable encoder and the data-URL prefix isn't part of it.
  // ponytail: the whole file crosses the bridge as base64 — fine at the sizes
  // the server hands back (images are re-encoded to <=2048px); swap in
  // Filesystem.downloadFile, which streams natively, if that ever hurts.
  function blobBase64(blob) {
    return new Promise((resolve, reject) => {
      const r = new FileReader();
      r.onloadend = () => resolve(String(r.result).split(',')[1] ?? '');
      r.onerror = () => reject(new Error('could not read the file'));
      r.readAsDataURL(blob);
    });
  }

  async function loadText(id) {
    const seq = ++textSeq;
    loadingText = true;
    textError = '';
    text = null;
    try {
      const body = await api.attachmentText(id);
      if (seq !== textSeq) return; // superseded
      text = body;
    } catch (e) {
      if (seq !== textSeq) return;
      textError = e?.message || 'Could not load this file';
    } finally {
      if (seq === textSeq) loadingText = false;
    }
  }

  // ---- image pane: pan + zoom ----
  // Pointer-event based, so one gesture model covers touch and mouse:
  //   1 pointer  -> drag to pan
  //   2 pointers -> pinch (the native-feeling mobile gesture)
  //   wheel      -> zoom anchored at the cursor
  //   double tap/click on the picture -> toggle 100% / 250%
  // The +/- buttons are pointer:fine-only; touch relies on the gestures.
  const MIN_SCALE = 1;
  const MAX_SCALE = 6;
  const DOUBLE_TAP_SCALE = 2.5;
  const TAP_MS = 300; // double-tap window
  const TAP_PX = 40; // max distance between the two taps
  const MOVE_PX = 3; // slop before a press counts as a drag
  const SWIPE_PX = 60; // minimum horizontal travel for a gallery flick
  const SWIPE_RATIO = 1.5; // how much wider than tall the flick must be

  let stage = $state(null); // the flex-centered layer the picture lives in
  let img = $state(null); // the <img>: offsetWidth/Height = layout size pre-transform
  let scale = $state(1);
  let tx = $state(0);
  let ty = $state(0);
  let imgLoaded = $state(false);
  let imgFailed = $state(false);
  // Button/double-tap zooms glide; an in-flight pinch would feel laggy with
  // a transition, so it's only applied while no gesture is running.
  let gesturing = $state(false);

  // Navigating (or reopening the viewer on a different file) must not inherit
  // the previous picture's zoom, offsets or load state.
  $effect(() => {
    if (!attachment?.id) return;
    scale = 1;
    tx = 0;
    ty = 0;
    imgLoaded = false;
    imgFailed = false;
  });

  // Warm the browser cache for the two neighbours so the next swipe or arrow
  // press is instant. The refs live in a plain array: a GC'd Image can abort
  // its fetch before the bytes land.
  const preloads = [];
  $effect(() => {
    if (!hasNav) return;
    for (const i of [navIndex - 1, navIndex + 1]) {
      const att = items[(((i % items.length) + items.length) % items.length)];
      if (att?.kind !== 'image') continue;
      const im = new Image();
      im.src = att.previewUrl || api.attachmentUrl(att.id);
      if (preloads.length >= 8) preloads.shift();
      preloads.push(im);
    }
  });

  const pointers = new Map(); // pointerId -> {x, y} in client coords
  let drag = null; // {id, x, y, tx, ty}
  let pinch = null; // {d, cx, cy, scale, tx, ty} — the gesture baseline
  // Tap bookkeeping (pointer capture retargets events at the stage, so the
  // click handler can't use e.target to tell picture from backdrop).
  let gestureHitImage = false;
  let gestureMoved = false;
  let lastTap = null; // {t, x, y}

  function clamp(v, lo, hi) {
    return Math.min(hi, Math.max(lo, v));
  }

  // Capture is best-effort: a throw (pointer already gone, synthetic event)
  // must not abort the gesture bookkeeping that follows.
  function capture(id) {
    try {
      stage?.setPointerCapture?.(id);
    } catch {
      /* ignore */
    }
  }

  function release(id) {
    try {
      if (stage?.hasPointerCapture?.(id)) stage.releasePointerCapture(id);
    } catch {
      /* ignore */
    }
  }

  // The picture is flex-centered, so its centre coincides with the stage
  // centre at scale 1 with no offset: a content point u (relative to the
  // centre, untransformed) lands on screen at p = c + t + s*u.
  function stageCentre() {
    const r = stage.getBoundingClientRect();
    return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
  }

  // Keep the picture inside the viewport once it's smaller than the stage.
  function clampOffsets(s = scale) {
    if (!img || !stage) return;
    const maxX = Math.max(0, (img.offsetWidth * s - stage.clientWidth) / 2);
    const maxY = Math.max(0, (img.offsetHeight * s - stage.clientHeight) / 2);
    tx = clamp(tx, -maxX, maxX);
    ty = clamp(ty, -maxY, maxY);
  }

  // Scale to s1 while keeping the content point under `from` pinned to `to`.
  // Reads the live transform, so it composes for incremental wheel zoom.
  function rescale(s1, from, to = from, s0 = scale, t0x = tx, t0y = ty) {
    const c = stageCentre();
    const k = s1 / s0;
    tx = to.x - c.x - k * (from.x - c.x - t0x);
    ty = to.y - c.y - k * (from.y - c.y - t0y);
    scale = s1;
    clampOffsets(s1);
  }

  function zoomBy(factor) {
    const c = stageCentre();
    const next = clamp(scale * factor, MIN_SCALE, MAX_SCALE);
    if (next !== scale) rescale(next, c);
  }

  function resetZoom() {
    scale = 1;
    tx = 0;
    ty = 0;
  }

  function pinchBaseline() {
    const [a, b] = [...pointers.values()];
    return {
      d: Math.hypot(a.x - b.x, a.y - b.y) || 1,
      cx: (a.x + b.x) / 2,
      cy: (a.y + b.y) / 2,
      scale,
      tx,
      ty,
    };
  }

  function onPointerDown(e) {
    if (e.button !== undefined && e.button > 0) return; // right/middle click
    if (pointers.size === 0) {
      gestureHitImage = false;
      gestureMoved = false;
    }
    // Pointer capture keeps move/up arriving when the finger slides over the
    // header, but it also retargets them at the stage — hence the flag above.
    if (e.target === img) gestureHitImage = true;
    capture(e.pointerId);
    pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
    gesturing = true;
    if (pointers.size === 2) {
      drag = null;
      pinch = pinchBaseline();
    } else if (pointers.size === 1) {
      pinch = null;
      drag = { id: e.pointerId, x: e.clientX, y: e.clientY, tx, ty };
    } else {
      pinch = null; // 3+ fingers: stop zooming, let go of the pan too
      drag = null;
    }
  }

  function onPointerMove(e) {
    if (!pointers.has(e.pointerId)) return;
    pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });

    if (pinch && pointers.size >= 2) {
      const p = pinchBaseline();
      const next = clamp(pinch.scale * (p.d / pinch.d), MIN_SCALE, MAX_SCALE);
      // Anchor on the ORIGINAL midpoint, so an off-centre pinch pans as it
      // zooms (the content under the first midpoint follows the fingers).
      rescale(next, { x: pinch.cx, y: pinch.cy }, { x: p.cx, y: p.cy }, pinch.scale, pinch.tx, pinch.ty);
      gestureMoved = true;
      return;
    }
    if (drag && e.pointerId === drag.id) {
      const dx = e.clientX - drag.x;
      const dy = e.clientY - drag.y;
      if (Math.hypot(dx, dy) > MOVE_PX) gestureMoved = true;
      tx = drag.tx + dx;
      ty = drag.ty + dy;
      clampOffsets();
    }
  }

  function onPointerUp(e) {
    if (!pointers.has(e.pointerId)) return;
    pointers.delete(e.pointerId);
    release(e.pointerId);
    if (pointers.size < 2) pinch = null;

    if (pointers.size === 1) {
      // One finger left a pinch: rebase the pan on the survivor so the
      // picture doesn't jump when it moves.
      const [id, p] = [...pointers.entries()][0];
      drag = { id, x: p.x, y: p.y, tx, ty };
      gestureMoved = true;
    }

    if (pointers.size > 0) return;
    // Capture the flick before the drag bookkeeping is reset: on an unzoomed
    // picture a horizontal drag is gallery navigation, anything else was a pan.
    const flick =
      drag && e.pointerId === drag.id
        ? { dx: e.clientX - drag.x, dy: e.clientY - drag.y }
        : null;
    drag = null;
    gesturing = false;
    clampOffsets();
    if (!gestureMoved) {
      handleTap(e);
      return;
    }
    if (flick) swipeNav(flick.dx, flick.dy);
  }

  // Gallery flick. Content follows the finger, so swiping left advances and
  // swiping right goes back. Once zoomed the same drag pans instead —
  // swiping away a picture you're inspecting would feel like losing it.
  function swipeNav(dx, dy) {
    if (!hasNav || scale !== MIN_SCALE) return;
    if (Math.abs(dx) < SWIPE_PX || Math.abs(dx) < Math.abs(dy) * SWIPE_RATIO)
      return;
    if (dx < 0) onnext?.();
    else onprev?.();
  }

  // Double tap/click on the picture toggles zoom. Detected manually because
  // dblclick is unreliable on touch (and touch-action:none suppresses the
  // browser's own double-tap-to-zoom).
  function handleTap(e) {
    const now = Date.now();
    const prev = lastTap;
    lastTap = { t: now, x: e.clientX, y: e.clientY };
    if (
      !prev ||
      now - prev.t > TAP_MS ||
      Math.hypot(e.clientX - prev.x, e.clientY - prev.y) > TAP_PX
    ) {
      return;
    }
    lastTap = null;
    if (scale > MIN_SCALE) {
      resetZoom();
      return;
    }
    // Zoom in centred on the tapped point rather than the middle.
    rescale(Math.min(MAX_SCALE, DOUBLE_TAP_SCALE), { x: e.clientX, y: e.clientY });
  }

  // Single press on the dark area AROUND the picture dismisses (the
  // lightbox convention). The picture itself is reserved for double-tap
  // zoom, so a first tap there must never close the viewer.
  function onStageClick() {
    if (gestureMoved || gestureHitImage || pointers.size > 0) return;
    close();
  }

  function onWheel(e) {
    if (!isImage) return;
    e.preventDefault();
    const next = clamp(scale * Math.exp(-e.deltaY * 0.002), MIN_SCALE, MAX_SCALE);
    if (next !== scale) rescale(next, { x: e.clientX, y: e.clientY });
  }

  // Text pane: tapping the wide gutter beside the text column dismisses,
  // but never when the press ended a selection started inside the text.
  let textWrap = $state(null);
  function onTextClick(e) {
    if (e.target !== textWrap) return;
    const sel = window.getSelection?.();
    if (sel && !sel.isCollapsed) return;
    close();
  }

  let imageStyle = $derived(
    `transform: translate3d(${tx}px, ${ty}px, 0) scale(${scale});` +
      (gesturing ? '' : ' transition: transform 160ms ease-out;'),
  );
  let zoomPct = $derived(`${Math.round(scale * 100)}%`);
</script>

<dialog
  bind:this={dlg}
  class="fixed inset-0 m-0 h-full max-h-none w-full max-w-none overflow-hidden border-0 p-0 {isImage
    ? 'bg-black/90 text-white [&::backdrop]:bg-black/70'
    : 'bg-background text-foreground [&::backdrop]:bg-background/70'} {closing ? 'closing' : ''}"
  aria-labelledby="attachment-viewer-title"
  onclose={closed}
  oncancel={(e) => {
    e.preventDefault();
    close();
  }}
  onkeydown={onKeydown}
>
  <!-- p-safe: keeps the header clear of the notch/status bar and the
       bottom edge clear of the Android nav bar (no-ops on desktop). -->
  <div class="flex h-full w-full flex-col p-safe">
    <header class="flex shrink-0 items-center gap-1.5 px-2 py-2 sm:px-3">
      <!-- Close first on touch (thumb reach, and the Android-app
           convention), last on desktop. -->
      <button
        type="button"
        class="{BTN} order-first [@media(pointer:fine)]:order-last"
        title="Close (Esc)"
        aria-label="Close viewer"
        onclick={close}
      >
        <X class="size-5" strokeWidth={1.75} aria-hidden="true" />
      </button>

      <div class="min-w-0 flex-1">
        <div id="attachment-viewer-title" class="truncate text-sm font-medium {isImage ? 'text-white/90' : 'text-foreground'}" title={attachment?.filename}>
          {attachment?.filename || 'Attachment'}
        </div>
        {#if subtitle}
          <div class="truncate text-[11px] tabular-nums {isImage ? 'text-white/45' : 'text-muted-foreground'}">{subtitle}</div>
        {/if}
      </div>

      {#if isImage}
        <!-- Zoom controls: pointer:fine only — touch uses pinch and
             double-tap, which is the gesture users expect on a phone. -->
        <div class="hidden items-center gap-0.5 [@media(pointer:fine)]:flex">
          <button
            type="button"
            class="{BTN} disabled:pointer-events-none disabled:opacity-25"
            title="Zoom out"
            aria-label="Zoom out"
            disabled={scale <= MIN_SCALE}
            onclick={() => zoomBy(1 / 1.4)}
          >
            <ZoomOut class="size-[18px]" strokeWidth={1.75} aria-hidden="true" />
          </button>
          <button
            type="button"
            class="min-w-14 rounded-full px-1 py-1.5 text-center font-mono text-xs tabular-nums text-white/70 transition-colors hover:bg-white/15 hover:text-white"
            title="Reset zoom"
            aria-label="Reset zoom to 100%"
            onclick={resetZoom}
          >
            {zoomPct}
          </button>
          <button
            type="button"
            class="{BTN} disabled:pointer-events-none disabled:opacity-25"
            title="Zoom in"
            aria-label="Zoom in"
            disabled={scale >= MAX_SCALE}
            onclick={() => zoomBy(1.4)}
          >
            <ZoomIn class="size-[18px]" strokeWidth={1.75} aria-hidden="true" />
          </button>
        </div>
      {/if}

      {#if !isImage && text}
        <button
          type="button"
          class={BTN}
          title="Copy contents"
          aria-label="Copy file contents"
          onclick={copyContents}
        >
          {#if copied}<Check class="size-[18px]" strokeWidth={1.75} aria-hidden="true" />{:else}<Copy class="size-[18px]" strokeWidth={1.75} aria-hidden="true" />{/if}
        </button>
      {/if}

      {#if native}
        <button
          type="button"
          class="{BTN} disabled:pointer-events-none disabled:opacity-40"
          title="Share"
          aria-label="Share file"
          disabled={sharing}
          onclick={share}
        >
          <Share2 class="size-5" strokeWidth={1.75} aria-hidden="true" />
        </button>
      {/if}

      <!-- Stays a plain URL anchor on purpose: same-origin downloads work
           through the `download` attribute, and on native (where the server
           is cross-origin) the system browser takes over and handles the
           save. The server also sets Content-Disposition for text files. -->
      <a
        class={BTN}
        href={url}
        download={attachment?.filename}
        target="_blank"
        rel="noopener noreferrer"
        title={`Download ${attachment?.filename ?? ''}`.trim()}
        aria-label="Download file"
      >
        <Download class="size-5" strokeWidth={1.75} aria-hidden="true" />
      </a>
    </header>

    {#if isImage}
      <div class="relative min-h-0 flex-1">
        <div
          bind:this={stage}
          class="absolute inset-0 flex touch-none select-none items-center justify-center overflow-hidden [-webkit-touch-callout:none]"
          role="presentation"
          onpointerdown={onPointerDown}
          onpointermove={onPointerMove}
          onpointerup={onPointerUp}
          onpointercancel={onPointerUp}
          onclick={onStageClick}
          onwheel={onWheel}
        >
          <img
            bind:this={img}
            src={url}
            alt={attachment?.filename ?? ''}
            draggable="false"
            class="max-h-full max-w-full shrink-0 object-contain {imgLoaded ? '' : 'invisible'}"
            style={imageStyle}
            onload={() => {
              imgLoaded = true;
              clampOffsets();
            }}
            onerror={() => (imgFailed = true)}
          />
        </div>

        {#if hasNav}
          <button
            type="button"
            class="{NAV_BTN} left-1.5"
            title="Previous image (←)"
            aria-label="Previous image"
            onclick={() => onprev?.()}
          >
            <ChevronLeft class="size-6" strokeWidth={1.75} aria-hidden="true" />
          </button>
          <button
            type="button"
            class="{NAV_BTN} right-1.5"
            title="Next image (→)"
            aria-label="Next image"
            onclick={() => onnext?.()}
          >
            <ChevronRight class="size-6" strokeWidth={1.75} aria-hidden="true" />
          </button>
        {/if}

        {#if imgFailed}
          <div class="pointer-events-none absolute inset-0 flex items-center justify-center px-6 text-center">
            <p class="text-sm text-white/60">This image could not be loaded.</p>
          </div>
        {:else if !imgLoaded}
          <div class="pointer-events-none absolute inset-0 flex items-center justify-center">
            <span
              role="status"
              aria-label="Loading image"
              class="inline-block size-6 animate-spin rounded-full border-2 border-white/25 border-t-white"
            ></span>
          </div>
        {/if}

        {#if imgLoaded && scale === MIN_SCALE}
          <!-- Touch hint: gestures are invisible, so say them once. Hidden
               on hover-capable devices, which get the +/- buttons instead,
               and once zoomed, where it would sit on top of the picture.
               No nowrap: on narrow phones the sentence wraps to two lines
               instead of bleeding off both screen edges. -->
        {/if}
      </div>
    {:else}
      <!-- The gutter click is a convenience dismissal only (✕ / Esc / Android
           back are the real, keyboard-reachable paths), and this div is a
           scroll container, not a control. -->
      <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
      <div
        bind:this={textWrap}
        class="min-h-0 flex-1 overflow-y-auto overscroll-contain px-3 pb-4 pt-2 sm:px-6"
        onclick={onTextClick}
      >
        {#if loadingText}
          <div class="flex h-full items-center justify-center">
            <span
              role="status"
              aria-label="Loading file"
              class="inline-block size-6 animate-spin rounded-full border-2 border-foreground/25 border-t-foreground"
            ></span>
          </div>
        {:else if textError}
          <div class="mx-auto max-w-md py-16 text-center">
            <p class="text-sm text-muted-foreground">{textError}</p>
            <button
              type="button"
              class="mt-4 rounded-full bg-accent px-4 py-1.5 text-sm text-accent-foreground transition-colors hover:bg-accent/70"
              onclick={() => loadText(attachment.id)}
            >
              Try again
            </button>
          </div>
        {:else}
          <!-- The server serves every text attachment as text/plain, so this
               is inert text — never HTML. Soft-wrapped so long lines never
               need a horizontal scroll. -->
          <pre class="mx-auto w-full max-w-4xl font-mono text-[13px] leading-relaxed whitespace-pre-wrap break-words text-foreground/85">{text}</pre>
        {/if}
      </div>
    {/if}
  </div>
</dialog>

<style>
  /* Open/close animation for the top-layer dialog + its ::backdrop, with a
     slight zoom on the content pane. Fullscreen overlay, so fade+zoom rather
     than a slide: scaling edge-to-edge chrome would flash the page behind. */
  dialog[open] { animation: av-in 180ms ease-out; }
  dialog[open]::backdrop { animation: av-in 180ms ease-out; }
  dialog[open] > div { animation: av-zoom-in 180ms ease-out; }
  dialog.closing,
  dialog.closing::backdrop { animation: av-out 150ms ease-in forwards; }
  dialog.closing > div { animation: av-zoom-out 150ms ease-in forwards; }
  @keyframes av-in {
    from { opacity: 0; }
  }
  @keyframes av-out {
    to { opacity: 0; }
  }
  @keyframes av-zoom-in {
    from { transform: scale(0.97); }
  }
  @keyframes av-zoom-out {
    to { transform: scale(0.97); }
  }
  @media (prefers-reduced-motion: reduce) {
    dialog[open],
    dialog[open]::backdrop,
    dialog[open] > div,
    dialog.closing,
    dialog.closing::backdrop,
    dialog.closing > div { animation: none; }
  }
</style>
