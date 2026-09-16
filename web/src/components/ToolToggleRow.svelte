<script>
  // One row of a tool toggle list: the switch, the tool name and its
  // description (falling back to the tool's origin server when it has none).
  // Shared by the per-chat Tools panel and the settings overlay's two default
  // lists (integrated tools, and each MCP server card's own tools).
  //
  // onTitle adds the optional user-facing title box (settings MCP cards only —
  // integrated titles are hardcoded in the backend). Its placeholder is the
  // title the tool already has, so an empty box shows what the chat displays.
  //
  // The name/description column carries a chevron and expands to the full
  // description, so the row reads as clickable.
  //
  // disabled greys the row out and kills the switch: the per-chat panel does
  // that for a specialist tool the picked chat model makes pointless.
  import { ChevronDown } from '@lucide/svelte';
  import { Switch } from '$lib/components/ui/switch';
  import { Input } from '$lib/components/ui/input';

  let { tool, checked, onToggle, titleValue = '', onTitle = null, disabled = false } = $props();

  let expanded = $state(false);
</script>

<div
  class="flex items-center gap-3 rounded-md p-2 transition-colors hover:bg-accent/50 {disabled
    ? 'opacity-50'
    : ''}"
>
  <div class="flex shrink-0 items-center">
    <Switch
      checked={checked}
      {disabled}
      aria-label="{tool.name} enabled"
      onCheckedChange={onToggle}
    />
  </div>
  <button
    type="button"
    class="flex min-w-0 flex-1 items-center gap-2 text-left"
    aria-expanded={expanded}
    onclick={() => (expanded = !expanded)}
  >
    <div class="min-w-0 flex-1">
      <div class="truncate text-sm leading-5 font-medium">{tool.name}</div>
      <div class="{expanded ? '' : 'line-clamp-2'} text-xs text-muted-foreground">
        {tool.description || tool.server}
      </div>
    </div>
    <ChevronDown
      class="size-4 shrink-0 text-muted-foreground transition-transform {expanded ? 'rotate-180' : ''}"
      strokeWidth={1.75}
      aria-hidden="true"
    />
  </button>
  {#if onTitle}
    <div class="w-36 shrink-0">
      <Input
        type="text"
        class="h-7 text-xs"
        placeholder={tool.title || 'Title (optional)'}
        title="Optional label the chat shows for this tool instead of its name"
        aria-label="User-facing title for {tool.name}"
        value={titleValue}
        oninput={(e) => onTitle(e.currentTarget.value)}
      />
    </div>
  {/if}
</div>
