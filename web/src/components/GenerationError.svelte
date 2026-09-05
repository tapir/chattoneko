<script>
  // THE single error display for anything that ends or cuts off a response
  // mid-generation: provider errors, the stop button, server shutdowns,
  // persistence failures… Every terminal problem renders through this exact
  // same component with a short, human explanation.
  import { app } from '../lib/state.svelte.js';
  import { RotateCcw, X } from '@lucide/svelte';
  import { Button } from '$lib/components/ui/button';

  let { status = 'failed', error = '' } = $props();

  let stopped = $derived(status === 'stopped');

  let title = $derived(stopped ? 'Generation stopped' : 'Generation failed');
  let explanation = $derived(
    error ||
      (stopped
        ? 'The response was stopped before it finished.'
        : 'The response ended before it finished. Try again.'),
  );
</script>

<div
  role="alert"
  class="mt-2 flex items-start gap-3 rounded-lg border border-destructive/40 bg-destructive/10 p-3 text-sm"
>
  <X class="mt-0.5 size-4 shrink-0 text-destructive" strokeWidth={1.75} aria-hidden="true" />
  <div class="min-w-0 flex-1">
    <div class="font-semibold text-destructive">{title}</div>
    <div class="mt-0.5 break-words text-muted-foreground">{explanation}</div>
  </div>
  <Button variant="outline" size="sm" class="shrink-0" onclick={() => app.regenerate()}>
    <RotateCcw class="size-4" strokeWidth={1.75} aria-hidden="true" />
    Retry
  </Button>
</div>
