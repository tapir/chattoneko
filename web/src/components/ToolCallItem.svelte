<script>
  // Expandable tool-call activity item: "Using tool X…" with arguments + result.
  import { Wrench } from '@lucide/svelte';
  import CollapsibleStatus from './CollapsibleStatus.svelte';

  let { call, status = '' } = $props();

  // A call still pending on a message that is no longer generating was cut
  // off (stop/failure/disconnect) — render it as failed, not spinning forever.
  let interrupted = $derived(call.pending && status !== 'generating');
  let isError = $derived(interrupted || call.is_error || (call.result || '').startsWith('Error:'));
  let running = $derived(call.pending && !interrupted);

  function pretty(text) {
    if (!text) return '';
    try {
      return JSON.stringify(JSON.parse(text), null, 2);
    } catch {
      return text;
    }
  }
</script>

<CollapsibleStatus
  icon={Wrench}
  title={call.name || '…'}
  {running}
  error={isError}
  errorBadge={!call.pending}
  class="mb-1.5"
>
  <div class="space-y-2">
    {#if call.args}
      <div>
        <div class="mb-0.5 text-xs font-semibold text-muted-foreground">Arguments</div>
        <pre class="overflow-x-auto rounded-md bg-muted p-2 text-xs whitespace-pre-wrap break-all">{pretty(call.args)}</pre>
      </div>
    {/if}
    {#if call.result}
      <div>
        <div class="mb-0.5 text-xs font-semibold text-muted-foreground">Result</div>
        <pre class="max-h-64 overflow-y-auto rounded-md bg-muted p-2 text-xs whitespace-pre-wrap break-all">{call.result}</pre>
      </div>
    {/if}
    {#if interrupted}
      <div class="text-xs text-muted-foreground">Interrupted before a result was received.</div>
    {:else if call.pending && !call.args}
      <div class="text-xs text-muted-foreground">Waiting for arguments…</div>
    {/if}
  </div>
</CollapsibleStatus>
