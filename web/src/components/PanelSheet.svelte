<script>
  // Right-side panel sheet shared by Logs / Tools / System prompt.
  // Owns the user-resizable width (persisted under `storageKey`) and the
  // left-edge drag handle, so callers only supply trigger + body snippets.
  // Mobile (<sm): no resize handle, the sheet is truly fullscreen
  // (100% width — the max-sm !important overrides beat the inline
  // width/max-width below). At every size the sheet is opened from the
  // top bar's 3-dot menu via the bound `open` prop — the trigger is
  // rendered hidden so the Sheet keeps its open/close machinery.
  import * as Sheet from '$lib/components/ui/sheet';
  import ResizeHandle from './ResizeHandle.svelte';
  import { registerOverlay } from '../lib/overlays.svelte.js';

  let {
    storageKey,
    title,
    description = '',
    trigger, // snippet
    headerExtra = null, // optional snippet rendered next to the title
    children,
    disabled = false,
    triggerTitle = '',
    open = $bindable(false),
    onOpenChange = null,
  } = $props();

  let width = $state(448);

  function handleOpenChange(next) {
    open = next;
    onOpenChange?.(next);
  }

  // Native Android back button: while open, this sheet is the topmost
  // overlay, so back closes it (registerOverlay's return value is the
  // $effect cleanup that unregisters on close / unmount).
  $effect(() => {
    if (open) return registerOverlay(() => (open = false));
  });
</script>

<Sheet.Root {open} onOpenChange={handleOpenChange}>
  <Sheet.Trigger
    class="hidden"
    title={triggerTitle}
    {disabled}
  >
    {@render trigger()}
  </Sheet.Trigger>
  <Sheet.Content side="right" class="w-full gap-0 max-sm:w-full! max-sm:max-w-full! sm:max-w-none" style="width: {width}px; max-width: 94vw;">
    <div class="max-sm:hidden">
      <ResizeHandle bind:width {storageKey} invert label="Resize {title} panel" />
    </div>
    <Sheet.Header>
      <Sheet.Title class="flex items-center gap-2">
        {title}
        {#if headerExtra}{@render headerExtra()}{/if}
      </Sheet.Title>
      {#if description}
        <Sheet.Description>{description}</Sheet.Description>
      {/if}
    </Sheet.Header>
    <div class="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
      {@render children()}
    </div>
  </Sheet.Content>
</Sheet.Root>
