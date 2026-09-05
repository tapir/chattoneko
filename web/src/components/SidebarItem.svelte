<script>
  import { app } from '../lib/state.svelte.js';
  import { Trash2 } from '@lucide/svelte';
  import IconButton from './IconButton.svelte';
  import ConfirmModal from './ConfirmModal.svelte';
  import ConfirmSheet from './ConfirmSheet.svelte';

  // revealed/onreveal: mobile long-press delete affordance. The parent
  // sidebar owns which row is revealed so only one chat at a time shows its
  // delete icon; revealing another row hides the previous one.
  let { chat, active, revealed = false, onreveal = null } = $props();

  // Breathing title (#3): pulse the title while this chat has an active
  // generation. The active chat reflects app.generating; other chats reflect
  // app.chatGeneratingIds, reconciled against server-side live generation
  // state (background chats have no stream to learn about completion from).
  let isGenerating = $derived(
    chat.id === app.activeChatId
      ? app.generating
      : app.chatGeneratingIds.has(chat.id),
  );

  let confirmingDelete = $state(false);
  // Decided at open time: below the sm breakpoint the confirmation renders
  // as a bottom action sheet (mobile-native), otherwise as a centered dialog.
  let deleteViaSheet = $state(false);

  // Long-press (touch) reveals the delete icon. The row is an <a>, so the
  // click that follows a long-press must be suppressed to avoid navigating.
  let lpTimer = null;
  let lpFired = false;
  let lpX = 0;
  let lpY = 0;
  const LP_DELAY = 500;
  const LP_TOLERANCE = 10; // px of finger drift that cancels the press

  function lpCancel() {
    if (lpTimer !== null) {
      clearTimeout(lpTimer);
      lpTimer = null;
    }
  }
  function lpTouchStart(e) {
    lpCancel();
    lpFired = false;
    const t = e.touches[0];
    lpX = t.clientX;
    lpY = t.clientY;
    lpTimer = setTimeout(() => {
      lpTimer = null;
      lpFired = true;
      navigator.vibrate?.(10);
      onreveal?.(chat.id);
    }, LP_DELAY);
  }
  function lpTouchMove(e) {
    if (lpTimer === null) return;
    const t = e.touches[0];
    if (Math.abs(t.clientX - lpX) > LP_TOLERANCE || Math.abs(t.clientY - lpY) > LP_TOLERANCE) {
      lpCancel();
    }
  }
  // Android Chrome long-press fires the native context menu on <a href>;
  // select-none/-webkit-touch-callout only cover text selection and iOS.
  // Suppress it so the long-press reveal is the only affordance.
  function onContextMenu(e) {
    e.preventDefault();
  }
  function onAnchorClick(e) {
    if (lpFired) {
      // This click is the tail end of a long-press: reveal only, don't open.
      e.preventDefault();
      lpFired = false;
      return;
    }
    // A normal tap on the revealed row dismisses the icon before navigating.
    if (revealed) onreveal?.(null);
  }
  // Stop a pending long-press timer if the row unmounts (deleted, list
  // refreshed) before the delay elapses.
  $effect(() => lpCancel);

  function displayTitle() {
    return chat.title || 'New Chat';
  }

  function openDelete(e) {
    e.preventDefault();
    e.stopPropagation();
    deleteViaSheet = matchMedia('(max-width: 639px)').matches;
    confirmingDelete = true;
  }
</script>

<li>
  <a
    href="#/c/{chat.id}"
    oncontextmenu={onContextMenu}
    ontouchstart={lpTouchStart}
    ontouchmove={lpTouchMove}
    ontouchend={lpCancel}
    ontouchcancel={lpCancel}
    onclick={onAnchorClick}
    class={[
      // select-none + no callout: without them a long-press also triggers
      // text selection / the iOS link preview on top of our reveal.
      'group relative flex select-none items-center gap-1 rounded-md px-2.5 py-2 text-sm transition-colors [-webkit-touch-callout:none]',
      active
        ? 'bg-sidebar-accent font-medium text-sidebar-accent-foreground'
        : // The revealed row takes the accent background too: touch browsers
          // can leave :hover stuck on the long-pressed row, and the delete
          // overlay's gradient must always match the row's background.
          revealed
          ? 'bg-sidebar-accent text-sidebar-accent-foreground'
          : 'text-sidebar-foreground/80 hover:bg-sidebar-accent hover:text-sidebar-accent-foreground'
    ]}
  >
    <span class={['min-w-0 flex-1 truncate', isGenerating && 'breathing']}>{displayTitle()}</span>
    <!-- Absolutely positioned overlay so the row never reflows when the
         actions appear on hover. No background/gradient here: the row
         itself carries the hover color, and a separate overlay background
         would lag behind the row's color transition and show as a patch. -->
    <span
      class={[
        'absolute right-1 top-1/2 flex -translate-y-1/2 items-center gap-0.5 rounded-md transition-opacity',
        // Hover reveal only on devices with a real pointer: touch browsers
        // leave :hover stuck after a tap, which would pin the icon open on
        // the last-touched row. On touch the long-press reveal governs.
        revealed
          ? 'opacity-100'
          : 'opacity-0 [@media(hover:hover)]:group-hover:opacity-100 group-focus-within:opacity-100'
      ]}
    >
      <IconButton icon={Trash2} label="Delete" size="sm" danger onclick={openDelete} />
    </span>
  </a>
</li>

{#if confirmingDelete}
  {@const props = {
    title: 'Delete chat?',
    body: `"${displayTitle()}" and all its messages will be permanently deleted.`,
    onconfirm: () => { confirmingDelete = false; app.deleteChat(chat.id); },
    oncancel: () => (confirmingDelete = false),
  }}
  {#if deleteViaSheet}
    <ConfirmSheet {...props} confirmLabel="Delete Chat" />
  {:else}
    <ConfirmModal {...props} />
  {/if}
{/if}
