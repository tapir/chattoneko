<script>
  import * as Drawer from '$lib/components/ui/drawer';
  import * as ToggleGroup from '$lib/components/ui/toggle-group';
  import { Check, ChevronDown } from '@lucide/svelte';

  // Mobile picker: a single centered model chip in the composer bar opens a
  // bottom drawer (vaul) holding both pickers — a segmented reasoning-effort
  // control and large tappable model rows. The popover Select used on desktop
  // is too small for touch (targets < 44px). Desktop keeps the popover Select.
  let {
    models = [],
    currentModel = '',
    currentEffort = '',
    effortOptions = [],
    onModelChange,
    onEffortChange,
  } = $props();

  let open = $state(false);

  // Writable bridge between the app state and the ToggleGroup. Tapping the
  // active segment in a type="single" group fires a change with an empty
  // value (deselect) — not a valid effort state, and at least one segment
  // must always stay pressed. The group mutates its bound value, so mirror
  // it into local state and restore the app's effort whenever it reports
  // empty (the re-push re-syncs the group).
  // Initialized empty; the sync effect below fills it in on mount, so the
  // prop is only ever read reactively (no stale initial capture).
  let effortValue = $state('');
  $effect(() => {
    effortValue = currentEffort;
  });
  $effect(() => {
    if (effortValue === '') effortValue = currentEffort;
  });

  // Picking an effort applies immediately and closes the drawer; picking a
  // model keeps it open so the user can also adjust effort in one visit.
  function handleEffortChange(value) {
    effortValue = value;
    if (!value) return;
    onEffortChange?.(value);
    open = false;
  }

  function handleModelSelect(model) {
    onModelChange?.(model);
  }

  // Borderless ghost chip — it is the only control in the mobile bar, so the
  // pill frame is unnecessary visual weight.
  const chipClass =
    'inline-flex h-9 min-w-0 max-w-72 items-center gap-1.5 rounded-full px-3 font-mono text-xs text-muted-foreground hover:bg-accent hover:text-accent-foreground';
</script>

<button type="button" class={chipClass} onclick={() => (open = true)}>
  <span class="min-w-0 truncate">{currentModel || 'Select model'}</span>
  {#if effortOptions.length > 0}
    <span class="shrink-0">- {currentEffort}</span>
  {/if}
  <ChevronDown class="size-3.5 shrink-0" strokeWidth={1.75} aria-hidden="true" />
</button>

<Drawer.Root bind:open>
  <!-- Bottom safe-area padding comes from the drawer-content primitive. -->
  <Drawer.Content>
    <!-- Title is required for a11y; nothing visible above the sections. -->
    <Drawer.Title class="sr-only">Model settings</Drawer.Title>

    <!-- Sections share a 16px left edge: model label/row text px-2 inside a
         px-2 scroller, effort label/segment px-4. -->
    <div class="min-h-0 flex-1 overflow-y-auto px-2 pb-2 pt-1">
      <div class="px-2 pb-1 text-xs font-medium text-muted-foreground">Model</div>
      {#each models as model (model)}
        <button
          type="button"
          class="flex w-full items-center gap-3 rounded-lg px-2 py-3 text-left hover:bg-accent"
          onclick={() => handleModelSelect(model)}
        >
          <span class="flex-1 truncate font-mono text-[0.8125rem]">{model}</span>
          {#if model === currentModel}
            <Check class="size-4 shrink-0 text-primary" strokeWidth={1.75} aria-hidden="true" />
          {/if}
        </button>
      {/each}
    </div>

    {#if effortOptions.length > 0}
      <div class="px-4 pb-1 pt-1">
        <div class="mb-2 text-xs font-medium text-muted-foreground">Reasoning effort</div>
        <ToggleGroup.Root
          type="single"
          variant="outline"
          bind:value={effortValue}
          onValueChange={handleEffortChange}
          class="w-full flex-wrap"
        >
          {#each effortOptions as effort (effort)}
            <ToggleGroup.Item value={effort} class="h-11 min-w-14 flex-1">
              <span class="truncate">{effort}</span>
            </ToggleGroup.Item>
          {/each}
        </ToggleGroup.Root>
      </div>
    {/if}
  </Drawer.Content>
</Drawer.Root>
