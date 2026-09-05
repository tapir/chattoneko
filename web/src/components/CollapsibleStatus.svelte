<script>
  // Shared collapsible status row: icon + title + status indicator + chevron,
  // with a default-open/closed <details> body. Used by ThinkingBlock and
  // ToolCallItem so both get identical chrome (shimmer title + spinner while
  // running, check/x when done, error tint on failure).
  //
  // Props:
  //   icon      — leading glyph (lucide component: Brain, Wrench, ...)
  //   title     — summary label (truncated)
  //   running   — currently streaming/executing (shimmer title + spinner)
  //   error     — terminal failure (red tint + x icon)
  //   errorBadge — show a destructive "error" Badge next to the status icon
  //   class     — extra container classes (spacing, etc.)
  import Spinner from './Spinner.svelte';
  import { Badge } from '$lib/components/ui/badge';
  import { X, Check, ChevronDown } from '@lucide/svelte';

  let {
    icon: Icon,
    title,
    running = false,
    error = false,
    errorBadge = false,
    class: cls = '',
    children,
  } = $props();
</script>

<details
  class={[
    'group rounded-lg border text-sm',
    error ? 'border-destructive/40 bg-destructive/5' : 'bg-muted/50',
    cls,
  ]}
>
  <summary class="flex list-none items-center gap-2 px-3 py-2.5 font-medium text-muted-foreground select-none [&::-webkit-details-marker]:hidden">
    <Icon class="size-3.5 shrink-0 opacity-60" strokeWidth={1.75} aria-hidden="true" />
    <span class="truncate" class:shimmer-text={running}>{title}</span>
    {#if running}
      <Spinner class="size-3.5 shrink-0" label={title} />
    {:else if error}
      <X class="size-3.5 shrink-0 text-destructive" strokeWidth={1.75} aria-hidden="true" />
    {:else}
      <Check class="size-3.5 shrink-0 text-success" strokeWidth={1.75} aria-hidden="true" />
    {/if}
    {#if error && errorBadge}
      <Badge variant="destructive" class="shrink-0">error</Badge>
    {/if}
    <ChevronDown class="ml-auto size-3.5 shrink-0 text-muted-foreground transition-transform group-open:rotate-180" strokeWidth={1.75} aria-hidden="true" />
  </summary>
  <div class="px-3 pb-2.5">
    {@render children()}
  </div>
</details>
