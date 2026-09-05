<script module>
  // Picked ONCE per app session (module scope), not per component instance.
  // Sending the first message flips the route (#/ → #/c/<id>), which remounts
  // ChatView/MessageList via the {#key} in App.svelte; an instance-scoped
  // pick would roll a different cat during the send round-trip.
  const nekos = Object.values(
    import.meta.glob('../lib/neko*.png', { eager: true, query: '?url', import: 'default' })
  );
  const neko = nekos[Math.floor(Math.random() * nekos.length)];
</script>

<script>
  import { app } from '../lib/state.svelte.js';
  import { ArrowDown } from '@lucide/svelte';
  import MessageItem from './MessageItem.svelte';
  import { Button } from '$lib/components/ui/button';

  // Build display items: tool-result messages are folded into the preceding
  // assistant message's tool calls; tool messages don't render standalone.
  let items = $derived.by(() => {
    // Pass 1: collect tool results. They are persisted AFTER the assistant
    // message that requested them (higher seq), so a single forward pass
    // would never match them and calls would render as pending forever.
    const toolResults = {};
    for (const m of app.messages) {
      if (m.role === 'tool') toolResults[m.tool_call_id] = m;
    }
    // Pass 2: build display items.
    const out = [];
    for (const m of app.messages) {
      if (m.role === 'tool') continue;
      if (m.role === 'assistant') {
        const calls = (m.tool_calls ?? []).map((tc) => {
          const res = toolResults[tc.provider_call_id];
          return {
            call_id: tc.provider_call_id,
            name: tc.name,
            args: tc.arguments,
            result: res?.content ?? '',
            is_error: res ? (res.content || '').startsWith('Error:') : false,
            pending: !res
          };
        });
        out.push({ msg: m, toolCalls: calls, key: m.id });
      } else {
        out.push({ msg: m, toolCalls: [], key: m.id });
      }
    }
    // fold live tool calls into the live assistant placeholder
    if (app.live) {
      const idx = out.findIndex((it) => it.msg.id === app.live.messageId);
      if (idx >= 0) out[idx].toolCalls = app.live.toolCalls;
    }
    return out;
  });

  let lastAssistantId = $derived.by(() => {
    for (let i = items.length - 1; i >= 0; i--) {
      if (items[i].msg.role === 'assistant') return items[i].msg.id;
    }
    return null;
  });

  // ---- auto-scroll with jump-to-bottom affordance ----
  // Pinned = follow the stream. ANY deliberate upward scroll (wheel, touch
  // drag, keyboard) unpins instantly so the user takes control; re-pin only
  // when they scroll back to the bottom or hit the jump button.
  let container = $state(null);
  let pinned = $state(true);
  // Whether the conversation is actually taller than the viewport — the
  // jump-to-bottom button is pointless (and distracting) otherwise.
  let canScroll = $state(false);
  let lastTop = 0;
  let lastHeight = 0;
  let touchStartY = null;

  function isNearBottom() {
    if (!container) return true;
    return container.scrollHeight - container.scrollTop - container.clientHeight < 80;
  }

  function updateCanScroll() {
    if (!container) return;
    canScroll = container.scrollHeight - container.clientHeight > 4;
  }

  // Content changes (stream growth, chat switch) can make the pane scrollable
  // or remove scrollability entirely.
  $effect(() => {
    items.length;
    requestAnimationFrame(updateCanScroll);
  });

  function onScroll() {
    if (!container) return;
    updateCanScroll();
    const top = container.scrollTop;
    const height = container.scrollHeight;
    if (height < lastHeight) {
      // Content shrank (chat switch, history reset): the browser clamps
      // scrollTop, which looks like an upward scroll but isn't user intent.
    } else if (top < lastTop - 1) {
      pinned = false;
    } else if (top > lastTop + 1 && isNearBottom()) {
      // Re-pin ONLY on deliberate downward movement into the near-bottom
      // zone. Without the `top > lastTop` guard, any no-op scroll event
      // (browser clamp adjustment, scroll anchoring, sub-pixel wobble) would
      // re-pin — and when the whole scrollable range is smaller than the 80px
      // near-bottom threshold, EVERY position is "near bottom", so the pane
      // would fight the user's upward scrolls and vibrate.
      pinned = true;
    }
    lastTop = top;
    lastHeight = height;
  }

  function onWheel(e) {
    if (e.deltaY < 0) pinned = false;
  }

  function onTouchStart(e) {
    touchStartY = e.touches[0]?.clientY ?? null;
  }

  function onTouchMove(e) {
    if (touchStartY == null) return;
    const y = e.touches[0]?.clientY;
    // Dragging a finger down pulls the content down = scrolling toward the top.
    if (y != null && y > touchStartY + 8) pinned = false;
  }

  function scrollToBottom() {
    if (!container) return;
    pinned = true;
    container.scrollTop = container.scrollHeight;
    lastTop = container.scrollTop;
    lastHeight = container.scrollHeight;
  }

  function trackContent() {
    updateCanScroll();
    if (!pinned || !container) return;
    requestAnimationFrame(() => {
      if (!container || !pinned) return;
      container.scrollTop = container.scrollHeight;
      lastTop = container.scrollTop;
      lastHeight = container.scrollHeight;
    });
  }
</script>

<div class="relative min-h-0 flex-1">
  <div
    bind:this={container}
    role="main"
    class="absolute inset-0 overflow-y-auto px-5 sm:px-6"
    onscroll={onScroll}
    onwheel={onWheel}
    ontouchstart={onTouchStart}
    ontouchmove={onTouchMove}
  >
    <div class="mx-auto max-w-4xl pb-4">
      {#if items.length === 0}
        <div class="flex min-h-[60vh] flex-col items-center justify-center py-16 text-center">
          <img src={neko} alt="neko" class="mb-4 h-24 w-auto" />
          <h1 class="text-xl font-semibold tracking-tight">How can I てつだう?</h1>
        </div>
      {/if}
      {#each items as item (item.key)}
        <MessageItem
          {item}
          isLive={!!app.live && app.live.messageId === item.msg.id}
          isLastAssistant={item.msg.id === lastAssistantId}
          track={trackContent}
        />
      {/each}
      <div class="h-2"></div>
    </div>
  </div>

  {#if !pinned && canScroll}
    <Button
      variant="outline"
      size="icon"
      class="pop-in bounce-hint absolute bottom-4 left-1/2 size-9 -translate-x-1/2 rounded-full border-border bg-background shadow-md hover:bg-accent hover:text-accent-foreground dark:bg-background dark:hover:bg-accent"
      title="Jump to bottom"
      onclick={scrollToBottom}
    >
      <ArrowDown class="size-4" strokeWidth={1.75} aria-hidden="true" />
    </Button>
  {/if}
</div>
