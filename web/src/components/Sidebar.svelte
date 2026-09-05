<script>
  import { app } from '../lib/state.svelte.js';
import logoUrl from '$lib/logo.svg';
  import { api } from '../lib/api.js';
  import { Plus, RotateCcw, Search, X } from '@lucide/svelte';
  import SidebarItem from './SidebarItem.svelte';
  import Spinner from './Spinner.svelte';
  import { Button } from '$lib/components/ui/button';
  import { Input } from '$lib/components/ui/input';
  import { Skeleton } from '$lib/components/ui/skeleton';

  // Set when rendered inside the mobile sheet: the header then shows a
  // close (X) button in the same flex row as New, so it stays vertically
  // centered with it instead of overlapping (the sheet's own absolute X is
  // disabled there).
  let { onClose = null } = $props();

  // Search (#4): debounce input -> store.runSearch; clearing restores recents.
  let searchTimer = null;
  function onSearchInput(e) {
    const q = e.target.value;
    app.searchQuery = q;
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => app.runSearch(q), 250);
  }
  function clearSearch() {
    clearTimeout(searchTimer);
    app.clearSearch();
  }
  // Which list to show: search results while a query is active, else recents.
  let displayedChats = $derived(app.searchResults ?? app.chats);
  let searching = $derived(app.searchResults !== null);

  // Mobile long-press delete: the id of the single row currently showing
  // its delete icon. Revealing another row replaces it (hiding the previous
  // one); null clears. Shared by both the recents and search-results lists.
  let deleteRevealId = $state(null);

  // Refresh the chat list from the database: desktop header button, or
  // pull-to-refresh on the list below <sm (where the button is hidden).
  let refreshing = $state(false);
  async function refreshChats() {
    if (refreshing) return;
    refreshing = true;
    try {
      await app.loadChats();
    } finally {
      refreshing = false;
    }
  }

  // Pull-to-refresh: dragging the list down from the top (touch) refreshes.
  let listEl = $state(null);
  let pull = $state(0);
  let pulling = $state(false);
  let touchStartY = null;
  const PTR_THRESHOLD = 56; // pull distance that commits a refresh on release
  const PTR_MAX = 96; // rubber-band cap

  function ptrStart(e) {
    touchStartY = listEl && listEl.scrollTop === 0 ? e.touches[0].clientY : null;
  }
  function ptrMove(e) {
    if (touchStartY == null || refreshing) return;
    const dy = e.touches[0].clientY - touchStartY;
    if (dy > 0 && listEl.scrollTop === 0) {
      e.preventDefault(); // we own the gesture; the list must not scroll
      pulling = true;
      pull = Math.min(dy * 0.45, PTR_MAX);
    } else {
      pulling = false;
      pull = 0;
    }
  }
  async function ptrEnd() {
    if (touchStartY == null) return;
    touchStartY = null;
    pulling = false;
    if (pull >= PTR_THRESHOLD) {
      pull = PTR_THRESHOLD; // hold the indicator open while loading
      await refreshChats();
    }
    pull = 0;
  }
</script>

<!-- border-r only on lg+ (desktop inline sidebar). In the mobile sheet the
     sidebar is fullscreen, where a right border rendered as a stray 1px
     line at the right screen edge. -->
<aside class="flex h-full w-full shrink-0 flex-col border-r-0 border-sidebar-border bg-sidebar text-sidebar-foreground lg:border-r">
  <div class="p-2.5 space-y-2">
    <!-- Header row: app title + new-chat / refresh-from-database actions -->
    <div class="flex items-center justify-between gap-8 px-1 py-1">
      <span class="inline-flex shrink-0 items-center gap-2 whitespace-nowrap pl-1 text-xl font-semibold tracking-tight">
        <img src={logoUrl} alt="Chattoねこ logo" class="size-6" />
        Chattoねこ
      </span>
      <div class="flex shrink-0 items-center gap-2">
        <a
          href="#/"
          onclick={() => {
            clearSearch();
            // Already on #/ there is no hashchange, so App's applyRoute
            // never runs to close the mobile sheet — close it here too.
            onClose?.();
          }}
          class="inline-flex h-9 items-center gap-1.5 rounded-md border border-sidebar-border bg-transparent px-2.5 text-sm font-medium shadow-none transition-colors hover:bg-sidebar-accent"
        >
          <Plus class="size-4" strokeWidth={1.75} aria-hidden="true" />
          New
        </a>
        <button
          class="inline-flex size-9 items-center justify-center rounded-md border border-sidebar-border bg-transparent shadow-none transition-colors hover:bg-sidebar-accent max-sm:hidden"
          title="Refresh chat list from database"
          aria-label="Refresh chat list from database"
          onclick={refreshChats}
          disabled={refreshing}
        >
          <RotateCcw class={refreshing ? 'size-4 animate-spin' : 'size-4'} strokeWidth={1.75} aria-hidden="true" />
        </button>
        {#if onClose}
          <button
            class="inline-flex size-9 items-center justify-center rounded-md border border-sidebar-border bg-transparent shadow-none transition-colors hover:bg-sidebar-accent"
            title="Close sidebar"
            aria-label="Close sidebar"
            onclick={onClose}
          >
            <X class="size-4" strokeWidth={1.75} aria-hidden="true" />
          </button>
        {/if}
      </div>
    </div>

    <!-- Search conversations by title (#4) -->
    <div class="relative">
      <Search class="absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" strokeWidth={1.75} aria-hidden="true" />
      <Input
        type="search"
        placeholder="Search chats…"
        value={app.searchQuery}
        oninput={onSearchInput}
        class="h-9 pl-8 pr-7 text-sm"
        aria-label="Search conversations"
      />
      {#if app.searchQuery}
        <button
          class="absolute right-1.5 top-1/2 flex size-5 -translate-y-1/2 items-center justify-center rounded text-muted-foreground hover:bg-sidebar-accent hover:text-sidebar-accent-foreground"
          title="Clear search"
          onclick={clearSearch}
          aria-label="Clear search"
        >
          <X class="size-3.5" strokeWidth={1.75} aria-hidden="true" />
        </button>
      {/if}
    </div>
  </div>

  <!-- Section heading: Recent conversations vs. Search results.
       Brighter than the chat titles (which render at /80 opacity). -->
  <div class="px-[18px] py-2.5 text-sm font-semibold tracking-wider text-sidebar-foreground">
    {#if searching}
      Search Results
    {:else}
      Recent Conversations
    {/if}
  </div>

  <!-- [contain:inline-size] excludes the (unbounded-length) chat titles from
       the sidebar's intrinsic width, so only the header row defines the
       content-derived min-width enforced in App.svelte. Safe here: the
       ConfirmModal dialog is top-layer (showModal) and the hover overlay is
       anchored to its relative <a> parent, neither relies on nav as a
       containing block. -->
  <nav
    bind:this={listEl}
    ontouchstart={ptrStart}
    ontouchmove={ptrMove}
    ontouchend={ptrEnd}
    ontouchcancel={ptrEnd}
    class="min-h-0 flex-1 overflow-y-auto px-2 pb-2 [contain:inline-size]"
  >
    <!-- Pull-to-refresh indicator (mobile): grows with the pull, spins while
         the refresh runs, collapses on release. -->
    <div
      class="overflow-hidden {!pulling ? 'transition-[height] duration-200 ease-out' : ''}"
      style="height: {pull}px"
    >
      <div class="flex h-14 items-center justify-center text-muted-foreground">
        {#if refreshing}
          <Spinner class="size-4" />
        {:else}
          <RotateCcw
            class="size-4"
            style="opacity: {Math.min(1, 0.35 + (pull / PTR_THRESHOLD) * 0.65)}"
            strokeWidth={1.75} aria-hidden="true"
          />
        {/if}
      </div>
    </div>
    {#if searching}
      {#if app.searchLoading}
        <div class="flex flex-col gap-1.5 px-1 pt-1">
          <Skeleton class="h-8 bg-sidebar-accent/60" />
          <Skeleton class="h-8 bg-sidebar-accent/60" />
        </div>
      {:else if displayedChats.length === 0}
        <p class="px-3 py-8 text-center text-sm text-muted-foreground">No chats match “{app.searchQuery}”</p>
      {:else}
        <ul class="flex flex-col gap-0.5">
          {#each displayedChats as chat (chat.id)}
            <SidebarItem {chat} active={chat.id === app.activeChatId} revealed={deleteRevealId === chat.id} onreveal={(id) => (deleteRevealId = id)} />
          {/each}
        </ul>
      {/if}
    {:else if app.chatsLoading && app.chats.length === 0}
      <div class="flex flex-col gap-1.5 px-1 pt-1">
        <Skeleton class="h-8 bg-sidebar-accent/60" />
        <Skeleton class="h-8 bg-sidebar-accent/60" />
        <Skeleton class="h-8 bg-sidebar-accent/60" />
      </div>
    {:else if app.chats.length === 0}
      <p class="px-3 py-8 text-center text-sm text-muted-foreground">No chats yet</p>
    {:else}
      <ul class="flex flex-col gap-0.5">
        {#each displayedChats as chat (chat.id)}
          <SidebarItem {chat} active={chat.id === app.activeChatId} revealed={deleteRevealId === chat.id} onreveal={(id) => (deleteRevealId = id)} />
        {/each}
      </ul>
    {/if}
  </nav>

  <!-- Pinned to the bottom of the sidebar (outside the scroll area) so it
       stays visible with margin no matter how long the chat list is. -->
  {#if !searching && app.chatsHasMore}
    <div class="shrink-0 px-2 pb-2.5 pt-1">
      <Button variant="ghost" size="sm" class="h-9 w-full text-muted-foreground" onclick={() => app.loadMoreChats()} disabled={app.chatsLoading}>
        {#if app.chatsLoading}
          <Spinner class="size-3.5" />
        {/if}
        Load more
      </Button>
    </div>
  {/if}
</aside>
