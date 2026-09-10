<script>
  import { app } from '../lib/state.svelte.js';
  import { formatTokens } from '../lib/format.js';
  import { themeState, toggleTheme } from '../lib/theme.svelte.js';
  import { EllipsisVertical, LogOut, Moon, PanelLeft, Plus, Server, Settings, Sun, Wrench } from '@lucide/svelte';
  import PanelSheet from './PanelSheet.svelte';
  import * as Popover from '$lib/components/ui/popover';
  import { Badge } from '$lib/components/ui/badge';
  import ToolToggleRow from './ToolToggleRow.svelte';
  import { registerOverlay } from '../lib/overlays.svelte.js';

  let chat = $derived(app.chat);
  let config = $derived(app.config);

  let enabledTools = $derived((config?.tools ?? []).filter((t) => app.toolEnabled(t)));

  // ---- theme ----
  // Reactive: tracks live system theme changes until an explicit choice is stored.
  let theme = $derived(themeState.current);

  // ---- model (for context-window stats; the picker lives in the Composer) ----
  let currentModel = $derived(chat ? (chat.model ?? '') : app.newChatModel);

  // ---- top-bar stats (#6) ----
  let promptTotal = $derived(app.chatUsage?.prompt_tokens ?? 0);
  let completionTotal = $derived(app.chatUsage?.completion_tokens ?? 0);
  let contextWindow = $derived(app.contextWindowFor(currentModel));
  // Context occupancy = final request's input+output of the last turn (what
  // the next request resends), NOT the billed totals: tool loops re-send full
  // history per request, so billed sums exceed the window while the context
  // stays inside it. Pre-migration rows lack context_tokens; for them the
  // billed sum equals the snapshot (single-request turns).
  let contextUsed = $derived.by(() => {
    for (let i = app.messages.length - 1; i >= 0; i--) {
      const m = app.messages[i];
      if (m.role === 'assistant' && (m.context_tokens || m.prompt_tokens))
        return m.context_tokens || (m.prompt_tokens ?? 0) + (m.completion_tokens ?? 0);
    }
    return 0;
  });
  let contextPct = $derived.by(() => {
    if (contextWindow <= 0) return null;
    const pct = Math.min(100, (contextUsed / contextWindow) * 100);
    // One decimal under 10% so non-trivial usage doesn't read as a flat "0%".
    return pct < 10 ? (Math.round(pct * 10) / 10).toFixed(1) : String(Math.round(pct));
  });

  // ---- top-bar menu: theme, Tools, Settings collapse
  // into a 3-dot menu at every size; mobile additionally gets a new-chat
  // button and moves the in/out token + context-usage stats (plus the theme
  // toggle, icon-only) into the menu's top row; desktop keeps them in the bar ----
  let mobileMenuOpen = $state(false);
  // Back button closes the open 3-dot menu like any other overlay.
  $effect(() => {
    if (mobileMenuOpen) return registerOverlay(() => (mobileMenuOpen = false));
  });
  let toolsOpen = $state(false);
</script>

{#snippet stats(cls, barCls = 'text-[9px]')}
  <!-- Token totals + context usage: header on desktop, 3-dot menu top row on mobile -->
  <div
    class="items-center gap-2.5 tabular-nums text-muted-foreground {cls}"
    title="Total input / output tokens for this chat"
  >
    <span class="flex items-center gap-0.5">
      <span aria-hidden="true">↑</span>{formatTokens(promptTotal)}
    </span>
    <span class="flex items-center gap-0.5">
      <span aria-hidden="true">↓</span>{formatTokens(completionTotal)}
    </span>
    {#if contextWindow > 0}
      <div
        class="relative h-4 w-16 overflow-hidden rounded-full border border-border bg-muted leading-none {barCls}"
        title="{contextUsed.toLocaleString()} of {contextWindow.toLocaleString()} tokens used"
        role="progressbar"
        aria-valuenow={Number(contextPct) || 0}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-label="Context window usage"
      >
        <div class="absolute inset-y-0 left-0 bg-primary/60" style="width: {Math.min(100, Number(contextPct) || 0)}%"></div>
        <span class="absolute inset-0 flex items-center justify-center tabular-nums">{contextPct === '0.0' ? '0' : contextPct}%</span>
        <!-- Same label clipped to the filled part, in the fill's contrast color,
             so the % stays readable over the fill at any width, both themes. -->
        <span
          aria-hidden="true"
          class="absolute inset-0 flex items-center justify-center tabular-nums text-primary-foreground"
          style="clip-path: inset(0 {100 - Math.min(100, Number(contextPct) || 0)}% 0 0)"
          >{contextPct === '0.0' ? '0' : contextPct}%</span>
      </div>
    {/if}
  </div>
{/snippet}

<header class="flex h-12 shrink-0 items-center gap-0.5 border-b bg-background/80 px-1.5 backdrop-blur sm:px-2">
  <!-- Left: sidebar toggle -->
  <div class="flex min-w-0 flex-1 items-center gap-0.5">
    <button
      data-sidebar="trigger"
      class="inline-flex size-9 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
      title="Toggle sidebar"
      aria-label="Toggle sidebar"
    >
      <PanelLeft class="size-[18px]" strokeWidth={1.75} aria-hidden="true" />
    </button>
  </div>

  <!-- Right: token/context stats (desktop), new chat (mobile), 3-dot menu -->
  <div class="flex items-center gap-0.5">
    <!-- Token totals + context usage (desktop only) -->
    {@render stats('hidden px-2 text-[11px] sm:flex')}

    <!-- Tools menu: available (MCP) tools + per-chat enable/disable -->
    <PanelSheet
      storageKey="chattoneko-tools-width"
      bind:open={toolsOpen}
      title="Tools"
      description="Tools available to the assistant in this chat. Toggles are saved for this chat; a new chat starts from the defaults in Settings."
    >
      <div class="flex flex-col gap-0.5">
        {#each config?.tools ?? [] as tool (tool.name)}
          <ToolToggleRow {tool} checked={app.toolEnabled(tool)} onToggle={(checked) => app.toggleTool(tool.name, checked)} />
        {:else}
          <p class="py-6 text-center text-sm text-muted-foreground">
            No tools available. Add MCP servers in the Settings overlay to give the assistant tools.
          </p>
        {/each}
      </div>
    </PanelSheet>

    <!-- Mobile-only: new chat straight from the top bar -->
    <a
      href="#/"
      class="inline-flex size-9 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground sm:hidden"
      title="New chat"
      aria-label="New chat"
    >
      <Plus class="size-[18px]" strokeWidth={1.75} aria-hidden="true" />
    </a>

    <!-- 3-dot menu: theme, Tools, Settings, Change Server, Log Out -->
    <Popover.Root bind:open={mobileMenuOpen}>
      <Popover.Trigger
        class="inline-flex size-9 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
        title="More actions"
        aria-label="More actions"
      >
        <EllipsisVertical class="size-[18px]" strokeWidth={1.75} aria-hidden="true" />
      </Popover.Trigger>
      <!-- Mobile: shrink-wrap to the stats row (with a floor so short rows still read as a menu); desktop keeps the fixed width -->
      <Popover.Content align="end" class="w-fit min-w-52 p-1.5 sm:w-64">
        <!-- Mobile-only top row: token/context stats left, theme toggle (icon only) right -->
        <div class="flex items-center sm:hidden">
          {@render stats('flex pl-3 text-[13px]', 'text-[10px]')}
          <!-- mr-0.5: optically centers the icon over the ~19px Tools count badge below,
               whose right edge is flush with the item padding but whose pill is faint. -->
          <button
            class="ml-auto mr-0.5 inline-flex size-9 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
            onclick={toggleTheme}
            title={theme === 'dark' ? 'Light mode' : 'Dark mode'}
            aria-label={theme === 'dark' ? 'Light mode' : 'Dark mode'}
          >
            {#if theme === 'dark'}<Sun class="size-4" strokeWidth={1.75} aria-hidden="true" />{:else}<Moon class="size-4" strokeWidth={1.75} aria-hidden="true" />{/if}
          </button>
        </div>
        <div class="my-0.5 h-px bg-border sm:hidden" role="separator"></div>
        <!-- Desktop keeps the full-width labelled theme item -->
        <button
          class="hidden w-full items-center gap-2.5 rounded-md px-2.5 py-2 text-sm transition-colors hover:bg-accent hover:text-accent-foreground sm:flex"
          onclick={toggleTheme}
        >
          {#if theme === 'dark'}<Sun class="size-4" strokeWidth={1.75} aria-hidden="true" />{:else}<Moon class="size-4" strokeWidth={1.75} aria-hidden="true" />{/if}
          {theme === 'dark' ? 'Light mode' : 'Dark mode'}
        </button>
        <button
          class="flex w-full items-center gap-2.5 rounded-md px-2.5 py-2 text-sm transition-colors hover:bg-accent hover:text-accent-foreground"
          onclick={() => {
            mobileMenuOpen = false;
            toolsOpen = true;
          }}
        >
          <Wrench class="size-4" strokeWidth={1.75} aria-hidden="true" />
          Tools
          {#if enabledTools.length > 0}
            <Badge variant="secondary" class="ml-auto px-1.5 text-[10px] tabular-nums">{enabledTools.length}</Badge>
          {/if}
        </button>
        <button
          class="flex w-full items-center gap-2.5 rounded-md px-2.5 py-2 text-sm transition-colors hover:bg-accent hover:text-accent-foreground"
          onclick={() => {
            mobileMenuOpen = false;
            app.settingsOpen = true;
          }}
        >
          <Settings class="size-4" strokeWidth={1.75} aria-hidden="true" />
          Settings
        </button>
        {#if app.nativeApp || app.authEnabled}
          <div class="my-0.5 h-px bg-border sm:my-1" role="separator"></div>
        {/if}
        {#if app.nativeApp}
          <button
            class="flex w-full items-center gap-2.5 rounded-md px-2.5 py-2 text-sm transition-colors hover:bg-accent hover:text-accent-foreground"
            onclick={() => {
              mobileMenuOpen = false;
              app.changeServer();
            }}
          >
            <Server class="size-4" strokeWidth={1.75} aria-hidden="true" />
            Change Server
          </button>
        {/if}
        {#if app.authEnabled}
          <button
            class="flex w-full items-center gap-2.5 rounded-md px-2.5 py-2 text-sm transition-colors hover:bg-accent hover:text-accent-foreground"
            onclick={() => {
              mobileMenuOpen = false;
              app.logout();
            }}
          >
            <LogOut class="size-4" strokeWidth={1.75} aria-hidden="true" />
            Log Out
          </button>
        {/if}
      </Popover.Content>
    </Popover.Root>
  </div>
</header>
