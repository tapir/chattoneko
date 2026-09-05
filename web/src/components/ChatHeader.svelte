<script>
  import { app } from '../lib/state.svelte.js';
  import { api } from '../lib/api.js';
  import { formatTokens } from '../lib/format.js';
  import { themeState, toggleTheme as toggleThemeMode } from '../lib/theme.svelte.js';
  import { EllipsisVertical, FileText, Info, LogOut, Moon, PanelLeft, Plus, Server, Settings, Sun, Wrench } from '@lucide/svelte';
  import CopyButton from './CopyButton.svelte';
  import Spinner from './Spinner.svelte';
  import PanelSheet from './PanelSheet.svelte';
  import * as Popover from '$lib/components/ui/popover';
  import { Badge } from '$lib/components/ui/badge';
  import { Switch } from '$lib/components/ui/switch';
  import { registerOverlay } from '../lib/overlays.svelte.js';

  let chat = $derived(app.chat);
  let config = $derived(app.config);

  let enabledTools = $derived((config?.tools ?? []).filter((t) => app.toolEnabled(t)));

  // ---- theme ----
  // Reactive: tracks live system theme changes until an explicit choice is stored.
  let theme = $derived(themeState.current);
  function toggleTheme() {
    toggleThemeMode();
  }

  // ---- model (for context-window stats; the picker lives in the Composer) ----
  let currentModel = $derived(chat ? (chat.model ?? '') : app.newChatModel);

  // ---- top-bar stats (#6) ----
  let promptTotal = $derived(app.chatUsage?.prompt_tokens ?? 0);
  let completionTotal = $derived(app.chatUsage?.completion_tokens ?? 0);
  let contextWindow = $derived(app.contextWindowFor(currentModel));
  let contextUsed = $derived(promptTotal + completionTotal);
  let contextPct = $derived.by(() => {
    if (contextWindow <= 0) return null;
    const pct = Math.min(100, (contextUsed / contextWindow) * 100);
    // One decimal under 10% so non-trivial usage doesn't read as a flat "0%".
    return pct < 10 ? (Math.round(pct * 10) / 10).toFixed(1) : String(Math.round(pct));
  });

  // ---- Logs (#6): plain-text conversation log in a side sheet ----
  let logText = $state('');
  let logError = $state('');
  let logLoading = $state(false);

  async function loadLog() {
    if (!chat) return;
    logLoading = true;
    logError = '';
    try {
      logText = await api.chatLog(chat.id);
    } catch (e) {
      logError = e?.message || 'Failed to load log';
    } finally {
      logLoading = false;
    }
  }

  // ---- top-bar menu: theme, Logs, Tools, System prompt, Settings collapse
  // into a 3-dot menu at every size; mobile additionally gets a new-chat
  // button, desktop keeps the in/out token + context-usage stats ----
  let mobileMenuOpen = $state(false);
  // Back button closes the open 3-dot menu like any other overlay.
  $effect(() => {
    if (mobileMenuOpen) return registerOverlay(() => (mobileMenuOpen = false));
  });
  let logsOpen = $state(false);
  let toolsOpen = $state(false);
  let systemOpen = $state(false);

  // Opening a sheet programmatically doesn't fire PanelSheet's onOpenChange,
  // so the log is loaded explicitly here.
  function openPanel(which) {
    mobileMenuOpen = false;
    if (which === 'logs') {
      logsOpen = true;
      loadLog();
    } else if (which === 'tools') {
      toolsOpen = true;
    } else {
      systemOpen = true;
    }
  }
</script>

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
    <div
      class="hidden items-center gap-2.5 px-2 text-[11px] tabular-nums text-muted-foreground sm:flex"
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
          class="relative h-4 w-16 overflow-hidden rounded-full border border-border bg-muted text-[9px] leading-none"
          title="{contextUsed.toLocaleString()} of {contextWindow.toLocaleString()} tokens used"
          role="progressbar"
          aria-valuenow={Number(contextPct) || 0}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-label="Context window usage"
        >
          <div class="absolute inset-y-0 left-0 bg-primary/60" style="width: {Math.min(100, Number(contextPct) || 0)}%"></div>
          <span class="absolute inset-0 flex items-center justify-center tabular-nums">{contextPct === '0.0' ? '0' : contextPct}%</span>
        </div>
      {/if}
    </div>

    <!-- Logs: plain-text debug log of the whole conversation -->
    <PanelSheet
      storageKey="chattoneko-logs-width"
      title="Logs"
      description="Plain-text debug log of the whole conversation — models, tool calls, usage, errors."
      triggerTitle="Full conversation log (models, tool calls, usage, errors)"
      disabled={!chat}
      bind:open={logsOpen}
      onOpenChange={(open) => open && loadLog()}
    >
      {#snippet trigger()}
        <FileText class="size-4" strokeWidth={1.75} aria-hidden="true" />
        <span class="hidden sm:inline">Logs</span>
      {/snippet}
      {#snippet headerExtra()}
        <CopyButton text={() => logText} label="Copy entire log to clipboard" size="sm" />
      {/snippet}
      {#if logLoading}
        <p class="flex items-center justify-center gap-2 py-6 text-sm text-muted-foreground"><Spinner class="size-4" /> Loading log…</p>
      {:else if logError}
        <p class="py-6 text-center text-sm text-destructive">{logError}</p>
      {:else if logText}
        <pre class="whitespace-pre-wrap rounded-md bg-muted p-3 font-mono text-xs leading-relaxed">{logText}</pre>
      {:else}
        <p class="py-6 text-center text-sm text-muted-foreground">Log is empty.</p>
      {/if}
    </PanelSheet>

    <!-- Tools menu: available (MCP) tools + per-chat enable/disable -->
    <PanelSheet
      storageKey="chattoneko-tools-width"
      bind:open={toolsOpen}
      title="Tools"
      triggerTitle="Tools available to the assistant in this chat"
      description="Tools available to the assistant in this chat. Toggles are saved for this chat; a new chat starts from the defaults in Settings."
    >
      {#snippet trigger()}
        <Wrench class="size-4" strokeWidth={1.75} aria-hidden="true" />
        <span class="hidden sm:inline">Tools</span>
        {#if enabledTools.length > 0}
          <Badge variant="secondary" class="px-1.5 text-[10px] tabular-nums">{enabledTools.length}</Badge>
        {/if}
      {/snippet}
      <div class="flex flex-col gap-0.5">
        {#each config?.tools ?? [] as tool (tool.name)}
          <div class="flex items-center gap-3 rounded-md p-2 transition-colors hover:bg-accent/50">
            <div class="flex shrink-0 items-center">
              <Switch
                checked={app.toolEnabled(tool)}
                onCheckedChange={(checked) => app.toggleTool(tool.name, checked)}
              />
            </div>
            <div class="min-w-0">
              <div class="truncate text-sm leading-5 font-medium">{tool.name}</div>
              <div class="line-clamp-2 text-xs text-muted-foreground">{tool.description || tool.server}</div>
            </div>
          </div>
        {:else}
          <p class="py-6 text-center text-sm text-muted-foreground">
            No tools available. Add MCP servers in the Settings overlay to give the assistant tools.
          </p>
        {/each}
      </div>
    </PanelSheet>

    <!-- System prompt (read-only): config prompt + enabled tool definitions -->
    <PanelSheet
      storageKey="chattoneko-system-width"
      bind:open={systemOpen}
      title="System prompt"
      triggerTitle="System prompt used for this chat"
      description="The effective system prompt sent with this conversation — the configured prompt plus the definitions of the enabled tools, so the model knows what it can call."
    >
      {#snippet trigger()}
        <Info class="size-4" strokeWidth={1.75} aria-hidden="true" />
        <span class="hidden sm:inline">System</span>
      {/snippet}
      {#if app.systemPrompt || config?.system_prompt}
        <pre class="whitespace-pre-wrap rounded-md bg-muted p-3 font-mono text-xs leading-relaxed">{app.systemPrompt || config?.system_prompt}</pre>
      {:else}
        <p class="text-sm text-muted-foreground">No system prompt is configured and no tools are enabled, so no system prompt is sent.</p>
      {/if}
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

    <!-- 3-dot menu: theme, Logs, Tools, System prompt, Settings, Change Server, Log Out -->
    <Popover.Root bind:open={mobileMenuOpen}>
      <Popover.Trigger
        class="inline-flex size-9 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
        title="More actions"
        aria-label="More actions"
      >
        <EllipsisVertical class="size-[18px]" strokeWidth={1.75} aria-hidden="true" />
      </Popover.Trigger>
      <Popover.Content align="end" class="w-64 p-1.5">
        <button
          class="flex w-full items-center gap-2.5 rounded-md px-2.5 py-2 text-sm transition-colors hover:bg-accent hover:text-accent-foreground"
          onclick={toggleTheme}
        >
          {#if theme === 'dark'}<Sun class="size-4" strokeWidth={1.75} aria-hidden="true" />{:else}<Moon class="size-4" strokeWidth={1.75} aria-hidden="true" />{/if}
          {theme === 'dark' ? 'Light mode' : 'Dark mode'}
        </button>
        <button
          class="flex w-full items-center gap-2.5 rounded-md px-2.5 py-2 text-sm transition-colors hover:bg-accent hover:text-accent-foreground disabled:opacity-40"
          disabled={!chat}
          onclick={() => openPanel('logs')}
        >
          <FileText class="size-4" strokeWidth={1.75} aria-hidden="true" />
          Logs
        </button>
        <button
          class="flex w-full items-center gap-2.5 rounded-md px-2.5 py-2 text-sm transition-colors hover:bg-accent hover:text-accent-foreground"
          onclick={() => openPanel('tools')}
        >
          <Wrench class="size-4" strokeWidth={1.75} aria-hidden="true" />
          Tools
          {#if enabledTools.length > 0}
            <Badge variant="secondary" class="ml-auto px-1.5 text-[10px] tabular-nums">{enabledTools.length}</Badge>
          {/if}
        </button>
        <button
          class="flex w-full items-center gap-2.5 rounded-md px-2.5 py-2 text-sm transition-colors hover:bg-accent hover:text-accent-foreground"
          onclick={() => openPanel('system')}
        >
          <Info class="size-4" strokeWidth={1.75} aria-hidden="true" />
          System prompt
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
          <div class="my-1 h-px bg-border" role="separator"></div>
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
