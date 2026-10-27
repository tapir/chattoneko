<script>
  // Thumbnail for a stored SVG attachment. The server keeps the file as text
  // and serves it text/plain, so the picture is a blob URL built from the
  // fetched source — the same trick the lightbox stage uses (lib/svg.js),
  // whose svgSource also gives the blob intrinsic dimensions so max-h/max-w
  // contain it exactly like a raster of the same size.
  import { FileText } from '@lucide/svelte';
  import { api } from '../lib/api.js';
  import { svgSource } from '../lib/svg.js';
  import { cn } from '../lib/utils.js';

  let { att, class: cls = '', ...rest } = $props();

  let url = $state('');
  // A .svg name is no guarantee: a misnamed or malformed file, or a fetch that
  // failed, leaves nothing to paint — a file tile stands in so the cell (and
  // its click, which opens the lightbox's text pane) still exists.
  let bad = $state(false);

  $effect(() => {
    const id = att?.id;
    if (!id) return;
    let blobUrl = '';
    let dead = false;
    api.attachmentText(id).then(
      (body) => {
        if (dead) return;
        const src = svgSource(body);
        if (!src) {
          bad = true;
          return;
        }
        blobUrl = URL.createObjectURL(new Blob([src], { type: 'image/svg+xml' }));
        url = blobUrl;
      },
      () => {
        if (!dead) bad = true;
      },
    );
    return () => {
      dead = true;
      if (blobUrl) URL.revokeObjectURL(blobUrl);
    };
  });
</script>

{#if url}
  <img src={url} alt={att?.filename ?? ''} class={cls} {...rest} />
{:else if bad}
  <span
    class={cn('flex size-24 items-center justify-center bg-muted text-muted-foreground', cls)}
    aria-hidden="true"
  >
    <FileText class="size-5" strokeWidth={1.75} aria-hidden="true" />
  </span>
{/if}
