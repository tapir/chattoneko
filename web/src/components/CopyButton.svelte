<script>
  // Icon button that copies `text` and flips to a check mark briefly.
  // `text` may be a string or a zero-arg function evaluated at click time
  // (so callers can pass live content without keeping a prop in sync).
  import { Check, X, Copy } from '@lucide/svelte';
  import IconButton from './IconButton.svelte';
  import { copyText } from '../lib/clipboard.js';

  let { text, label = 'Copy', size = 'md', class: cls = '' } = $props();

  let copied = $state(false);
  let failed = $state(false);
  let timer = null;

  async function copy() {
    const value = typeof text === 'function' ? text() : text;
    copied = await copyText(value);
    failed = !copied;
    clearTimeout(timer);
    timer = setTimeout(() => {
      copied = false;
      failed = false;
    }, 1200);
  }
</script>

<IconButton icon={copied ? Check : failed ? X : Copy} danger={failed} {label} onclick={copy} {size} class={cls} />
