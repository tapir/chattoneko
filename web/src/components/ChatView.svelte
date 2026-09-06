<script>
  import { app } from '../lib/state.svelte.js';
  import ChatHeader from './ChatHeader.svelte';
  import MessageList from './MessageList.svelte';
  import Composer from './Composer.svelte';
</script>

<div class="relative flex h-full min-h-0 flex-col bg-background">
  <ChatHeader />
  {#if app.chatLoading}
    <!-- The spacer holds the flex space MessageList would, so the Composer
         never jumps; the bar overlays the pane's top edge instead. -->
    <div class="flex-1"></div>
    <div class="chat-progress absolute inset-x-0 top-0 z-10" role="progressbar" aria-label="Loading chat">
      <div class="chat-progress-fill h-full bg-primary"></div>
    </div>
  {:else}
    <MessageList />
  {/if}
  <Composer />
</div>
