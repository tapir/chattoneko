<script module>
  // The recording currently playing, so starting one stops the last. A module
  // ref rather than a store: the players are the only readers.
  let current = null;
</script>

<script>
  // One audio attachment as a player row: play/pause, a scrub bar, a clock —
  // all of it the app's own chrome. The <audio> element is hidden and only does
  // the decoding: its built-in controls are browser-drawn (their own corners,
  // their own palette, no theme tokens) and their shadow root swallows the
  // pointer events lib/longpress.js needs.
  import { Pause, Play } from '@lucide/svelte';
  import { Button } from '$lib/components/ui/button';
  import { Slider } from '$lib/components/ui/slider';
  import { api } from '../lib/api.js';
  import { app } from '../lib/state.svelte.js';
  import { attachMenu } from '../lib/attachmenu.svelte.js';
  import { longPress } from '../lib/longpress.js';
  import { formatClock } from '../lib/format.js';
  import { onDestroy } from 'svelte';

  let {
    att, // AttachmentMeta ({id, filename, mime, …}) of a stored audio file
  } = $props();

  let el = $state(null);
  let playing = $state(false);
  let pos = $state(0);
  let duration = $state(0);

  // One label, always worth reading: the length while idle, the position once
  // it is moving. (The server answers audio under its real mime — see
  // docs/backend.md — which is what lets the element report a length at all.)
  let clock = $derived(playing || pos > 0 ? formatClock(pos) : formatClock(duration));
  let action = $derived(playing ? 'Pause' : 'Play');

  function toggle() {
    if (!el) return;
    if (playing) {
      el.pause();
      return;
    }
    if (current && current !== el) current.pause();
    // A refused play (dead link, codec this browser won't take) would otherwise
    // leave the button claiming it is playing.
    el.play().catch(() => app.toast('error', `Cannot play ${att.filename}`));
  }

  // The element reports its position ~4x a second and the slider steps in
  // 0.25s — the same resolution — so a position never has to be snapped onto a
  // step (a snapped value comes back through bind:value and fights playback).
  const quantize = (t) => Math.round(t * 4) / 4;

  // Only a COMMIT moves the playhead. onValueChange fires for our own
  // timeupdate writes too (the value round-trips through bind:value), and
  // seeking on those restarts playback from where it already is, 4x a second.
  function commit(v) {
    if (el) el.currentTime = v;
  }

  // The row can unmount mid-playback (chat switch, logout) with the detached
  // <audio> still going and the module ref pinning it, so nothing on screen
  // could stop it. `mine` rather than `el`: bind:this is already cleared by
  // the time onDestroy runs.
  let mine = null;
  onDestroy(() => {
    if (!mine) return;
    mine.pause();
    if (current === mine) current = null;
  });
</script>

<!-- The row is the long-press target, exactly like a picture or a file chip.
     The fill is a translucent black veil, not a surface token: every token
     surface is BRIGHTER than the page in dark mode, so black alpha is the only
     fill that reads "a tad darker" in both themes, over the bubble and the page. -->
<div
  class="flex w-64 max-w-full items-center gap-2 rounded-lg border bg-black/10 px-2 py-1.5 select-none [-webkit-touch-callout:none] sm:w-80"
  {...longPress(() => attachMenu.open(att))}
>
  <Button
    variant="default"
    size="icon"
    class="shrink-0 rounded-full"
    onclick={toggle}
    title={`${action} ${att.filename}`}
    aria-label={`${action} ${att.filename}`}
  >
    {#if playing}
      <Pause aria-hidden="true" />
    {:else}
      <Play aria-hidden="true" />
    {/if}
  </Button>
  <Slider
    bind:value={pos}
    max={duration || 1}
    step={0.25}
    onValueCommit={commit}
    label={`Seek ${att.filename}`}
    class="min-w-0 flex-1"
  />
  <span class="shrink-0 text-xs tabular-nums text-muted-foreground">{clock}</span>
</div>

<!-- preload="metadata" is the whole cost of a row at rest: a few header bytes
     for the length, no audio until play. -->
<audio
  bind:this={el}
  class="hidden"
  preload="metadata"
  src={api.attachmentUrl(att.id)}
  onplay={() => {
    playing = true;
    current = el;
    mine = el;
  }}
  onpause={() => {
    playing = false;
    if (current === el) current = null;
  }}
  onended={() => {
    playing = false;
    pos = 0;
    if (current === el) current = null;
  }}
  ontimeupdate={() => {
    // A drag owns the thumb until it commits: while the element still plays
    // from the old position its clock must not yank the bar back.
    if (el && Math.abs(el.currentTime - pos) < 0.5) pos = quantize(el.currentTime);
  }}
  onloadedmetadata={() => {
    if (el && Number.isFinite(el.duration)) duration = el.duration;
  }}
></audio>
