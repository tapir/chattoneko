<script>
  // One row of a tool toggle list: the switch, the tool name and its
  // description (falling back to the tool's origin server when it has none).
  // Shared by the per-chat Tools panel and the settings overlay's two default
  // lists (integrated tools, and each MCP server card's own tools).
  import { Switch } from '$lib/components/ui/switch';

  let { tool, checked, onToggle } = $props();
</script>

<div class="flex items-center gap-3 rounded-md p-2 transition-colors hover:bg-accent/50">
  <div class="flex shrink-0 items-center">
    <Switch checked={checked} aria-label="{tool.name} enabled" onCheckedChange={onToggle} />
  </div>
  <div class="min-w-0">
    <div class="truncate text-sm leading-5 font-medium">{tool.name}</div>
    <div class="line-clamp-2 text-xs text-muted-foreground">{tool.description || tool.server}</div>
  </div>
</div>
