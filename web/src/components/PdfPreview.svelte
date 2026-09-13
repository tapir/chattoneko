<script>
  // One page of a PDF, rendered by svelte-pdf (pdf.js) with all of the
  // library's own chrome switched off: its toolbar is a fixed teal-on-white
  // strip that knows nothing about the app's tokens, and both callers have
  // their own controls around it anyway. What's left is the canvas, which
  // this component stretches to whatever box it is placed in.
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { pdfjs } from '../lib/pdf.js';
  import { cn } from '../lib/utils.js';

  let {
    att, // AttachmentMeta ({id, filename, mime, …}) of a stored PDF
    page = 1,
    // Render scale in CSS pixels, NOT display size — the canvas is always
    // stretched to the box, so 2 is what keeps a fit-to-width page sharp on a
    // phone instead of upscaling a 1x raster. Thumbnails stay at 1: a 612px
    // page downscaled into a 150px card is already supersampled.
    scale = 2,
    // Intrinsic-size canvas instead of box-filling: the lightbox fits the
    // page with a transform (like a zoomed-out picture), so the canvas has to
    // keep its rendered size for the pan/zoom maths to have something true.
    natural = false,
    // Fires once the first page is painted (the lightbox drops its spinner
    // and measures the page then; svelte-pdf itself signals nothing).
    onready = null,
    class: cls = '',
  } = $props();

  let Pdf = $state(null);
  let mount = $state(null);
  let ready = $state(false);
  // <img>/<a> can't carry the Authorization header, so this is the ?token=
  // URL (the server accepts a token on GETs) — pdf.js fetches it plainly.
  let url = $derived(api.attachmentUrl(att.id));

  onMount(() => {
    let dead = false;
    // The shared worker has to be in place BEFORE the first getDocument, so
    // pdf.js loads first and the component mounts only once it has.
    pdfjs()
      .then(() => import('svelte-pdf'))
      .then((m) => {
        if (!dead) Pdf = m.default;
      })
      .catch(() => {}); // a failed load just leaves the placeholder

    // svelte-pdf signals nothing — it paints into a canvas it owns and never
    // says when. An unpainted canvas is fully transparent, so one corner
    // pixel's alpha is the whole "the page is on screen" signal (the element's
    // own default 300x150 size is NOT: it exists before anything renders).
    // Polled because a canvas paint fires no mutation to observe; capped so a
    // PDF that never paints (password prompt) doesn't tick forever.
    let ticks = 0;
    const poll = setInterval(() => {
      const c = mount?.querySelector('canvas');
      const painted = c && c.getContext('2d').getImageData(0, 0, 1, 1).data[3] > 0;
      if (painted || ++ticks > 300) {
        clearInterval(poll);
        if (painted) {
          ready = true;
          onready?.();
        }
      }
    }, 100);
    return () => {
      dead = true;
      clearInterval(poll);
    };
  });
</script>

<div
  bind:this={mount}
  class={cn('pdf', natural && 'pdf-natural', ready ? 'ready' : 'animate-pulse bg-muted', cls)}
>
  {#if Pdf}
    <!-- currentPage is the controlled prop (pageNum is its legacy alias); with
         showButtons empty the library renders no navigation of its own. -->
    <Pdf
      {url}
      {scale}
      currentPage={page}
      showButtons={[]}
      showBorder={false}
      showTopButton={false}
    />
  {/if}
</div>

<style>
  /* The library's own classes, so the overrides have to be :global. `.parent`
     carries a 1.25rem side margin (it expects to own the page) and the canvas
     is sized in rendered pixels — either one would break the box this is
     placed in. The placeholder holds a portrait page's shape so the card
     doesn't collapse to nothing and then jump. */
  .pdf:not(.ready) {
    aspect-ratio: 8.5 / 11;
  }
  .pdf :global(.parent) {
    margin: 0;
  }
  .pdf :global(canvas) {
    display: block;
    width: 100%;
    height: auto;
    /* Paper is white in both themes: a page with transparent margins would
       otherwise show the dark viewer's background through it. */
    background: #fff;
  }
  .pdf-natural :global(canvas) {
    width: auto;
    height: auto;
  }
</style>
