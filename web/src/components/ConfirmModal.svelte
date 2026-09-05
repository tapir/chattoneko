<script>
  import { onMount, onDestroy } from 'svelte';
  import { Button } from '$lib/components/ui/button';
  import { registerOverlay } from '../lib/overlays.svelte.js';

  let { title = 'Are you sure?', body = '', confirmLabel = 'Delete', onconfirm, oncancel } = $props();
  let dlg;
  // The confirm button is inside <form method="dialog">, so confirming also
  // fires the dialog's close event — guard against reporting both.
  let confirmed = false;

  let unregisterBack;
  onMount(() => {
    dlg?.showModal();
    // Native Android back button cancels the dialog (fires `close`, which
    // reports oncancel) instead of navigating away underneath it.
    unregisterBack = registerOverlay(() => dlg?.close());
  });
  onDestroy(() => unregisterBack?.());

  function confirm() {
    confirmed = true;
    onconfirm?.();
  }

  function closed() {
    if (!confirmed) oncancel?.();
  }
</script>

<dialog
  bind:this={dlg}
  class="fixed inset-0 z-50 m-auto w-full max-w-md rounded-xl border bg-card p-0 text-card-foreground shadow-xl backdrop:bg-black/60"
  onclose={closed}
>
  <div class="p-6">
    <h3 class="text-base font-semibold">{title}</h3>
    {#if body}
      <p class="mt-2 text-sm text-muted-foreground">{body}</p>
    {/if}
    <div class="mt-6 flex justify-end gap-2">
      <form method="dialog" class="contents">
        <Button variant="outline" type="submit">Cancel</Button>
        <Button variant="destructive" type="submit" onclick={confirm}>{confirmLabel}</Button>
      </form>
    </div>
  </div>
</dialog>
