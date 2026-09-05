<script>
  // Image attachments as a gallery instead of a ragged flex-wrap row: one
  // picture keeps its natural aspect ratio (nothing to tidy, and cropping a
  // lone image would be a regression), two or more become a grid of square
  // thumbnails. Columns come from CONTAINER queries, not viewport
  // breakpoints, because the chat column width is user-resizable (sidebar
  // drag handle) and collapses on mobile.
  //
  // Past `cap` images the last cell darkens into a "+N" tile — the message
  // stays scannable when a tool gathers a dozen pictures, and tapping the
  // tile opens the lightbox AT that image, where the rest are one swipe /
  // arrow-key away (viewer.open(att, items) hands the whole set over).
  //
  // `overlay` is an optional snippet rendered as a sibling of each cell's
  // button (never inside it), so controls like the "what the model saw"
  // badge can capture their own clicks without also opening the viewer.
  import { viewer } from '../lib/viewer.svelte.js';
  import { cn } from '../lib/utils.js';
  import AttachmentImage from './AttachmentImage.svelte';

  let {
    items, // image attachments ({id, filename, kind, ...}); see AttachmentImage for local previews
    cap = 6,
    singleClass = 'max-h-60', // max-height for the lone-image case
    // Explicit width for the grid when the parent is shrink-to-fit (the
    // right-aligned user bubble): a fr-track grid inside a fit-content
    // block collapses to min-content otherwise. Ignored for a lone image.
    widthClass = '',
    overlay = null, // optional {#snippet overlay(att)}
    class: cls = '',
  } = $props();

  let count = $derived(items?.length ?? 0);
  let visible = $derived(items?.slice(0, cap) ?? []);
  let hidden = $derived(count - visible.length);

  // Exactly 2 or 3 images get one cell each, so a phone never ends up with
  // a single orphaned cell on the last row; their thumbnails are still big
  // enough to recognize, and the lightbox has the full-size picture. 4+
  // fills whole rows (6 = 3x2 or 2x3) and reflows to 3-up when wide.
  let gridClass = $derived(
    count === 2
      ? 'grid-cols-2'
      : count === 3
        ? 'grid-cols-3'
        : 'grid-cols-2 @lg:grid-cols-3',
  );
</script>

{#if count > 0}
  <div class={cn('@container', count > 1 && widthClass, cls)}>
    {#if count === 1}
      <!-- Shrink-wrapped, not a grid cell: the badge overlay positions
           against the picture itself. -->
      <div class="relative inline-block">
        <button
          type="button"
          class="block cursor-zoom-in"
          title={`View ${items[0].filename}`}
          aria-label={`View ${items[0].filename}`}
          onclick={() => viewer.open(items[0], items)}
        >
          <AttachmentImage
            att={items[0]}
            class={cn('max-w-full rounded-lg border', singleClass)}
            loading="lazy"
          />
        </button>
        {#if overlay}{@render overlay(items[0])}{/if}
      </div>
    {:else}
      <div class={cn('grid gap-1.5', gridClass)}>
        {#each visible as att, i (att.id)}
          <div class="relative">
            <button
              type="button"
              class="relative block aspect-square w-full cursor-zoom-in overflow-hidden rounded-lg border bg-muted"
              title={`View ${att.filename}`}
              aria-label={
                i === visible.length - 1 && hidden > 0
                  ? `View ${att.filename} and ${hidden} more image${hidden === 1 ? '' : 's'}`
                  : `View ${att.filename}`
              }
              onclick={() => viewer.open(att, items)}
            >
              <AttachmentImage {att} class="size-full object-cover" loading="lazy" />
              {#if i === visible.length - 1 && hidden > 0}
                <span class="pointer-events-none absolute inset-0 flex items-center justify-center bg-black/55 text-sm font-medium tabular-nums text-white">
                  +{hidden}
                </span>
              {/if}
            </button>
            {#if overlay}{@render overlay(att)}{/if}
          </div>
        {/each}
      </div>
    {/if}
  </div>
{/if}
