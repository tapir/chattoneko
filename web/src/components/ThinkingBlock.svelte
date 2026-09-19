<script>
  // Collapsible "Thinking" block — collapsed by default, fed by reasoning
  // deltas while streaming and re-rendered from persisted reasoning on reload.
  import { Brain } from '@lucide/svelte';
  import CollapsibleStatus from './CollapsibleStatus.svelte';

  let { text, streaming = false, error = false } = $props();
</script>

<CollapsibleStatus icon={Brain} title="Thinking" running={streaming} {error} class="mb-2">
  {#if text}
    <div class="whitespace-pre-wrap font-mono text-xs leading-relaxed text-muted-foreground">{text}</div>
  {:else}
    <div class="text-xs italic text-muted-foreground/70">Waiting for thinking tokens…</div>
  {/if}
</CollapsibleStatus>
