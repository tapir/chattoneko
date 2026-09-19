<script>
  import * as Drawer from '$lib/components/ui/drawer';
  import { Camera, Image, FolderOpen } from '@lucide/svelte';
  import { registerOverlay } from '../lib/overlays.svelte.js';

  // Native-only attachment shortcuts (camera / gallery / file manager) in a
  // bottom drawer — same vaul pattern as ModelPickerSheet. The Composer owns
  // the picking logic; tapping a row closes the drawer immediately because
  // the native picker takes over the whole screen, and the picked media is
  // attached as soon as the user returns to the app.
  let { open = $bindable(false), busy = false, onAction } = $props();

  // Android back button closes the drawer before falling back to history
  // (LIFO overlay registry, same as PanelSheet/SettingsSheet).
  $effect(() => {
    if (open) return registerOverlay(() => (open = false));
  });

  const actions = [
    { kind: 'camera', icon: Camera, label: 'Camera', hint: 'Take a photo' },
    { kind: 'photos', icon: Image, label: 'Photos', hint: 'Pick from the gallery' },
    { kind: 'files', icon: FolderOpen, label: 'Files', hint: 'Documents, code and more' },
  ];
</script>

<Drawer.Root bind:open>
  <!-- Bottom safe-area padding comes from the drawer-content primitive. -->
  <Drawer.Content>
    <!-- Title is required for a11y; nothing visible above the rows. -->
    <Drawer.Title class="sr-only">Add attachment</Drawer.Title>

    <div class="min-h-0 flex-1 overflow-y-auto px-2 pb-2 pt-1">
      {#each actions as action (action.kind)}
        <button
          type="button"
          class="flex w-full items-center gap-3 rounded-lg px-2 py-3 text-left hover:bg-accent disabled:opacity-50"
          disabled={busy}
          onclick={() => onAction?.(action.kind)}
        >
          <span class="flex size-10 shrink-0 items-center justify-center rounded-full bg-accent">
            <action.icon class="size-5" strokeWidth={1.75} aria-hidden="true" />
          </span>
          <span class="min-w-0 flex-1">
            <span class="block text-sm">{action.label}</span>
            <span class="block truncate text-xs text-muted-foreground">{action.hint}</span>
          </span>
        </button>
      {/each}
    </div>
  </Drawer.Content>
</Drawer.Root>
