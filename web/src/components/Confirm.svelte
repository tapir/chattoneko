<script>
  // One confirmation, two layouts: below `sm` an iOS-style bottom action sheet
  // (vaul drawer, stacked thumb-height targets), above it a centered
  // <dialog>. The layout is picked at mount, which is when the caller asks the
  // question. Dismissing by ANY means — Cancel, overlay/backdrop, swipe down,
  // Escape, Android back — reports oncancel through one path; confirming
  // reports onconfirm and then dismisses, which the `confirmed` flag keeps
  // from being reported as a cancel too.
  import { onMount, onDestroy } from 'svelte';
  import * as Drawer from '$lib/components/ui/drawer';
  import { Button } from '$lib/components/ui/button';
  import { registerOverlay } from '../lib/overlays.svelte.js';

  let { title = 'Are you sure?', body = '', confirmLabel = 'Delete', onconfirm, oncancel } = $props();

  const sheet = matchMedia('(max-width: 639px)').matches;
  let dlg = $state();
  let open = $state(true); // sheet layout only
  let confirmed = false;

  function confirm() {
    confirmed = true;
    if (sheet) open = false;
    onconfirm?.();
  }

  function dismissed() {
    if (!confirmed) oncancel?.();
  }

  $effect(() => {
    if (sheet && !open) dismissed();
  });

  // Native Android back button dismisses instead of navigating away
  // underneath the question.
  let unregisterBack;
  onMount(() => {
    if (sheet) unregisterBack = registerOverlay(() => (open = false));
    else {
      dlg?.showModal();
      unregisterBack = registerOverlay(() => dlg?.close());
    }
  });
  onDestroy(() => unregisterBack?.());
</script>

{#if sheet}
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
{:else}
  <dialog
    bind:this={dlg}
    class="fixed inset-0 z-50 m-auto w-full max-w-md rounded-xl border bg-card p-0 text-card-foreground shadow-xl backdrop:bg-black/60"
    onclose={dismissed}
  >
    <div class="p-6">
      <h3 class="text-base font-semibold">{title}</h3>
      {#if body}
        <p class="mt-2 text-sm text-muted-foreground">{body}</p>
      {/if}
      <div class="mt-6 flex justify-end gap-2">
        <!-- The confirm button is inside <form method="dialog">, so
             confirming also fires the dialog's close event. -->
        <form method="dialog" class="contents">
          <Button variant="outline" type="submit">Cancel</Button>
          <Button variant="destructive" type="submit" onclick={confirm}>{confirmLabel}</Button>
        </form>
      </div>
    </div>
  </dialog>
{/if}
