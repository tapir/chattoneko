<script>
  // One row of a tool toggle list: the switch, the tool name and its
  // description (falling back to the tool's origin server when it has none).
  // Shared by the per-chat Tools panel and the settings overlay's two default
  // lists (integrated tools, and each MCP server card's own tools).
  //
  // Passing onTitle adds the optional user-facing title box (settings MCP
  // cards only — integrated titles are hardcoded in the backend). Its
  // placeholder is the title the tool already has, so an empty box shows what
  // the chat will display.
  import { Switch } from '$lib/components/ui/switch';
  import { Input } from '$lib/components/ui/input';

  let { tool, checked, onToggle, titleValue = '', onTitle = null } = $props();
</script>

<div class="flex items-center gap-3 rounded-md p-2 transition-colors hover:bg-accent/50">
  <div class="flex shrink-0 items-center">
    <Switch checked={checked} aria-label="{tool.name} enabled" onCheckedChange={onToggle} />
  </div>
  <div class="min-w-0 flex-1">
    <div class="truncate text-sm leading-5 font-medium">{tool.name}</div>
    <div class="line-clamp-2 text-xs text-muted-foreground">{tool.description || tool.server}</div>
  </div>
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
