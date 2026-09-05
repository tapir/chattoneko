<script>
  import { onMount, onDestroy } from 'svelte';
  import * as Drawer from '$lib/components/ui/drawer';
  import { Button } from '$lib/components/ui/button';
  import { registerOverlay } from '../lib/overlays.svelte.js';

  // Mobile counterpart of ConfirmModal: an iOS-style bottom action sheet
  // (vaul drawer) instead of a centered dialog. Dismissing by any means —
  // swipe down, overlay tap, Cancel button, Android back — reports oncancel
  // through the single `open` watcher. Desktop keeps the centered dialog;
  // the caller picks per viewport when opening.
  let { title = 'Are you sure?', body = '', confirmLabel = 'Delete', onconfirm, oncancel } = $props();

  let open = $state(true);
  let confirmed = false;

  function confirm() {
    confirmed = true;
    open = false;
    onconfirm?.();
  }

  $effect(() => {
    if (!open && !confirmed) oncancel?.();
  });

  // Native Android back button dismisses the sheet (sets open=false, which
  // reports oncancel) instead of navigating away underneath it.
  let unregisterBack;
  onMount(() => {
    unregisterBack = registerOverlay(() => (open = false));
  });
  onDestroy(() => unregisterBack?.());
</script>

<Drawer.Root bind:open>
  <!-- Bottom safe-area padding comes from the drawer-content primitive. -->
  <Drawer.Content class="px-4">
    <!-- Title is required for a11y; the visible header repeats it. -->
    <Drawer.Title class="sr-only">{title}</Drawer.Title>

    <div class="px-2 pb-4 pt-3 text-center">
      <h3 class="text-base font-semibold">{title}</h3>
      {#if body}
        <p class="mt-1 text-sm text-muted-foreground">{body}</p>
      {/if}
    </div>

    <!-- Stacked, full-width, thumb-height targets — the action-sheet layout
         (side-by-side buttons in a dialog are a desktop pattern). -->
    <div class="space-y-2">
      <Button variant="destructive" class="h-12 w-full rounded-xl text-base font-semibold" onclick={confirm}>
        {confirmLabel}
      </Button>
      <Button variant="secondary" class="h-12 w-full rounded-xl text-base font-medium" onclick={() => (open = false)}>
        Cancel
      </Button>
    </div>
  </Drawer.Content>
</Drawer.Root>
