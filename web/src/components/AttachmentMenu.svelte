<script>
  // Long-press an image or a file chip in the chat → Share / Copy: the two
  // things a phone wants with an attachment, where a hover-revealed action
  // row can't reach. Touch-only by construction (lib/longpress.js) — message
  // text keeps the browser's own long press for selecting and copying.
  //
  // Mounted once in App.svelte, driven by the attachMenu singleton.
  import * as Drawer from '$lib/components/ui/drawer';
  import { Copy, FileText, Share2 } from '@lucide/svelte';
  import { api } from '../lib/api.js';
  import { formatBytes } from '../lib/format.js';
  import { attachMenu } from '../lib/attachmenu.svelte.js';
  import { copyAttachment, shareAttachment } from '../lib/attach-actions.js';
  import { registerOverlay } from '../lib/overlays.svelte.js';

  let att = $derived(attachMenu.attachment);
  // What the sheet renders: the last real target, kept through the close
  // animation — a row tap nulls `attachment` at once, and emptying the sheet
  // on the way down reads as a glitch.
  let shown = $state(null);
  $effect(() => {
    if (att) shown = att;
  });
  let image = $derived(shown?.kind === 'image');

  // Android back closes the sheet before falling back to history (LIFO
  // overlay registry, same as AttachmentSheet/PanelSheet).
  $effect(() => {
    if (att) return registerOverlay(() => attachMenu.close());
  });

  let rows = $derived([
    { key: 'share', icon: Share2, label: 'Share', hint: 'Send it to another app', run: shareAttachment },
    {
      key: 'copy',
      icon: Copy,
      label: image ? 'Copy image' : 'Copy text',
      hint: image ? 'Put the picture on the clipboard' : 'Copy the file contents',
      run: copyAttachment,
    },
  ]);

  // A row fires and closes at once — the sheet has no business staying up
  // under the system share dialog. The work then runs on the tap's user
  // activation, which Chrome keeps for ~5s: enough for a LAN fetch, and
  // pre-fetching wouldn't help (same clock starts at the tap).
  function run(row) {
    const target = att;
    attachMenu.close();
    row.run(target);
  }
</script>

<Drawer.Root open={!!att} onOpenChange={(o) => !o && attachMenu.close()}>
  <!-- Bottom safe-area padding comes from the drawer-content primitive. -->
  <Drawer.Content>
    <!-- Title is required for a11y; the visible head is the file itself. -->
    <Drawer.Title class="sr-only">Attachment actions</Drawer.Title>

    <div class="px-2 pb-2 pt-1">
      <div class="flex min-w-0 items-center gap-3 px-2 pb-2.5 pt-1.5">
        <span class="flex size-10 shrink-0 items-center justify-center overflow-hidden rounded-full bg-accent">
          {#if image}
            <img src={api.attachmentUrl(shown.id)} alt="" class="size-full object-cover" />
          {:else}
            <FileText class="size-5" strokeWidth={1.75} aria-hidden="true" />
          {/if}
        </span>
        <span class="min-w-0 flex-1">
          <span class="block truncate text-sm">{shown?.filename}</span>
          {#if shown?.size}
            <span class="block text-xs text-muted-foreground">{formatBytes(shown.size)}</span>
          {/if}
        </span>
      </div>

      {#each rows as row (row.key)}
        <button
          type="button"
          class="flex w-full items-center gap-3 rounded-lg px-2 py-3 text-left hover:bg-accent"
          onclick={() => run(row)}
        >
          <span class="flex size-10 shrink-0 items-center justify-center rounded-full bg-accent">
            <row.icon class="size-5" strokeWidth={1.75} aria-hidden="true" />
          </span>
          <span class="min-w-0 flex-1">
            <span class="block text-sm">{row.label}</span>
            <span class="block truncate text-xs text-muted-foreground">{row.hint}</span>
          </span>
        </button>
      {/each}
    </div>
  </Drawer.Content>
</Drawer.Root>
