<script>
  import { app } from '../lib/state.svelte.js';
  import ChatHeader from './ChatHeader.svelte';
  import MessageList from './MessageList.svelte';
  import Composer from './Composer.svelte';
  import Spinner from './Spinner.svelte';
</script>

<div class="relative flex h-full min-h-0 flex-col bg-background">
  <ChatHeader />
  {#if app.chatLoading}
    <div class="flex flex-1 items-center justify-center">
      <Spinner class="size-7" label="Loading chat" />
    </div>
  {:else}
    <MessageList />
  {/if}
  <Composer />
</div>
