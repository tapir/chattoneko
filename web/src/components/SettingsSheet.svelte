<script>
  import { app } from '../lib/state.svelte.js';
  import { api } from '../lib/api.js';
  import { Download, Eye, EyeOff, MessageCircle, Trash2, X, Zap } from '@lucide/svelte';
  import Spinner from './Spinner.svelte';
  import ToolToggleRow from './ToolToggleRow.svelte';
  import { Button } from '$lib/components/ui/button';
  import { Input } from '$lib/components/ui/input';
  import { Label } from '$lib/components/ui/label';
  import * as Select from '$lib/components/ui/select';
  import * as ToggleGroup from '$lib/components/ui/toggle-group';
  import { registerOverlay } from '../lib/overlays.svelte.js';
  import { onDestroy } from 'svelte';

  // Server settings overlay. Sits on top of the main app.
  //
  // Forced-open contract: when the server reports setup_complete === false
  // (no provider endpoint/key and designated models yet) the overlay opens
  // automatically on top of everything and cannot be dismissed — no close
  // button, backdrop clicks and Escape are ignored — until a save makes the
  // config complete. Otherwise it opens/closes like a normal panel from the
  // sidebar or header.
  let open = $derived(app.settingsOpen || app.setupComplete === false);
  let canClose = $derived(app.setupComplete !== false);

  // Open/close animation is class-driven CSS (same trick as the lightbox):
  // Svelte transitions on this block stalled its unmount, so `visible` keeps
  // the DOM alive through the exit animation and then drops it.
  const REDUCED = matchMedia('(prefers-reduced-motion: reduce)').matches;
  let visible = $state(false); // synced from `open` by the effect below, incl. on mount
  let closing = $state(false);
  let closeTimer = null;
  $effect(() => {
    if (open) {
      clearTimeout(closeTimer);
      closing = false;
      visible = true;
    } else if (visible) {
      closing = true;
      closeTimer = setTimeout(() => {
        closing = false;
        visible = false;
      }, REDUCED ? 0 : 150);
    }
  });
  onDestroy(() => clearTimeout(closeTimer));

  let loading = $state(false);
  let saving = $state(false);
  let fetchingId = $state(''); // model card currently fetching data
  let adding = $state(false); // add-model fetch in flight
  let error = $state('');

  // ---- form state (loaded from GET /api/setup) ----
  let systemPrompt = $state('');
  let baseUrl = $state('');
  // Preloaded from the stored config and sent back as-is on save; the box
  // holds the real value, only visually masked until the eye icon reveals it.
  let apiKey = $state('');
  let showApiKey = $state(false);
  let uploadMaxBytes = $state('');
  let maxToolIter = $state('');
  let mcpTimeout = $state('');
  let mcpServers = $state([]);
  // Per-card MCP tool lists, keyed by the row's local key: what that card's
  // "Fetch" button last dialed. A card without an entry falls back to
  // the live catalog's tools for its server name (see toolsFor). Deliberately
  // NOT part of the dirty snapshot — it describes the remote server, not the
  // config; only the toggles (tool_defaults) are config.
  let serverTools = $state({});
  let fetchingServer = $state(''); // row key currently dialing
  // Global per-tool default toggles: sparse map (tool name → bool) of the
  // explicit overrides, integrated and MCP tools alike. Rows render from the
  // live catalog (app.config.tools) or a card's fetched list; a tool absent
  // from the map keeps its own default_enabled.
  let toolDefaults = $state({});
  // Global per-tool user-facing titles: sparse map (tool name → label) shown
  // in the chat instead of the raw name. MCP tools only — integrated titles
  // are hardcoded in the backend. Optional: an empty box keeps whatever the
  // catalog already reports.
  let toolTitles = $state({});

  // Models: one card per whitelisted model. The default chat/task models are
  // flagged from these cards — there are no separate inputs for them.
  let modelCards = $state([]);
  let defaultChatModel = $state('');
  let defaultTaskModel = $state('');
  let defaultVisionModel = $state('');
  let newModel = $state('');

  // Dirty tracking: a JSON snapshot of the whole form, compared against the
  // baseline taken when settings were (re)loaded. Save is disabled while
  // nothing changed.
  let baseline = $state('');
  let baselineMcp = $state('');
  let dirty = $derived(!loading && baseline !== '' && snapshot() !== baseline);

  // Adding models / fetching their data queries the provider, which needs a
  // base URL and an API key. Both fields are preloaded from the stored
  // config, so their (possibly unsaved) form values are the whole picture.
  let providerReady = $derived(baseUrl.trim() !== '' && apiKey.trim() !== '');
  // Unsaved provider form values forwarded to the fetch endpoint; the server
  // falls back to the stored config for any field left empty.
  function providerOverrides() {
    return { baseUrl: baseUrl.trim(), apiKey: apiKey.trim() };
  }

  const MODALITIES = ['text', 'image', 'audio'];
  const DEFAULT_EFFORTS = ['max', 'xhigh', 'high', 'medium', 'low', 'minimal', 'none'];
  const DEFAULT_EFFORT = 'medium';
  const DEFAULT_CONTEXT = 131072;
  // Entry.Server of an integrated tool (the backend's mcphub.BuiltinServer).
  const BUILTIN_SERVER = 'builtin';

  function snapshot() {
    return JSON.stringify({
      systemPrompt,
      baseUrl,
      apiKey,
      defaultChatModel,
      defaultTaskModel,
      defaultVisionModel,
      cards: modelCards,
      mcp: mcpServers,
      // Sorted so the comparison is deterministic regardless of toggle order.
      tools: Object.entries(toolDefaults).sort(([a], [b]) => a.localeCompare(b)),
      titles: Object.entries(toolTitles).sort(([a], [b]) => a.localeCompare(b)),
      limits: [uploadMaxBytes, maxToolIter, mcpTimeout],
    });
  }

  function applyConfig(c) {
    c = c ?? {};
    systemPrompt = c.system_prompt ?? '';
    baseUrl = c.provider?.base_url ?? '';
    apiKey = c.provider?.api_key ?? '';
    uploadMaxBytes = String(c.limits?.upload_max_file_bytes ?? '');
    maxToolIter = String(c.limits?.max_tool_iterations ?? '');
    mcpTimeout = String(c.limits?.mcp_call_timeout_seconds ?? '');
    defaultChatModel = c.models?.default_chat_model ?? '';
    defaultTaskModel = c.models?.default_task_model ?? '';
    defaultVisionModel = c.models?.default_vision_model ?? '';
    const metas = {};
    for (const m of c.models?.metas ?? []) metas[m.model_id] = m;
    modelCards = (c.models?.whitelist ?? []).map((id) => {
      const m = metas[id] ?? {};
      return {
        id,
        contextLength: String(m.context_length ?? DEFAULT_CONTEXT),
        inputModality: [...(m.input_modality ?? ['text'])],
        outputModality: [...(m.output_modality ?? ['text'])],
        reasoningEfforts: [...(m.reasoning_efforts ?? DEFAULT_EFFORTS)],
        effortOptions: [...(m.reasoning_efforts ?? DEFAULT_EFFORTS)],
        reasoningDefault: m.reasoning_default ?? DEFAULT_EFFORT,
      };
    });
    mcpServers = (c.mcp_servers ?? []).map(normalizeServer);
    // Fetched lists belong to the rows that dialed them; a (re)load rebuilds
    // every row, so drop the stale keys with them.
    serverTools = {};
    toolDefaults = { ...(c.tool_defaults ?? {}) };
    toolTitles = { ...(c.tool_titles ?? {}) };
    baseline = snapshot();
    baselineMcp = JSON.stringify(mcpServers);
  }

  // ---- load ----
  async function load() {
    loading = true;
    error = '';
    try {
      const data = await api.setup();
      applyConfig(data?.config ?? {});
    } catch (e) {
      error = e?.message || 'Failed to load settings';
    } finally {
      loading = false;
    }
  }

  // Reload every time the overlay transitions to open.
  $effect(() => {
    if (open) void load();
  });

  // ---- close handling ----
  function attemptClose() {
    if (canClose) app.settingsOpen = false;
  }
  function onKeydown(e) {
    if (!open) return; // svelte:window is always mounted; only act while open
    if (e.key === 'Escape') attemptClose();
  }
  // Android back button closes the overlay (no-op while forced open).
  $effect(() => {
    if (open) return registerOverlay(attemptClose);
  });

  // ---- models ----
  // Adds a whitelisted model: the card is pushed first, then filled by the
  // same provider fetch the per-card "fetch data" button uses (which also
  // persists the metadata server-side). A failed fetch removes the card again
  // rather than leaving a half-populated one behind.
  async function addModel() {
    const id = newModel.trim();
    if (!id || adding || fetchingId || !providerReady) return;
    if (modelCards.some((c) => c.id === id)) {
      app.toast('error', `${id} is already whitelisted`);
      return;
    }
    adding = true;
    modelCards = [
      {
        id,
        contextLength: String(DEFAULT_CONTEXT),
        inputModality: ['text'],
        outputModality: ['text'],
        reasoningEfforts: [...DEFAULT_EFFORTS],
        effortOptions: [...DEFAULT_EFFORTS],
        reasoningDefault: DEFAULT_EFFORT,
      },
      ...modelCards,
    ];
    newModel = '';
    try {
      if (!(await fetchModelData(id, { added: true }))) {
        modelCards = modelCards.filter((c) => c.id !== id);
      }
    } finally {
      adding = false;
    }
  }
  function removeModel(id) {
    modelCards = modelCards.filter((c) => c.id !== id);
    if (defaultChatModel === id) defaultChatModel = '';
    if (defaultTaskModel === id) defaultTaskModel = '';
    if (defaultVisionModel === id) defaultVisionModel = '';
  }
  function toggleDefaultChat(id) {
    defaultChatModel = defaultChatModel === id ? '' : id;
  }
  function toggleDefaultTask(id) {
    defaultTaskModel = defaultTaskModel === id ? '' : id;
  }
  function toggleDefaultVision(id) {
    defaultVisionModel = defaultVisionModel === id ? '' : id;
  }
  function setEfforts(card, efforts) {
    // At least one level must stay selected — an empty change is rejected by
    // re-pushing the current value (new reference) into the group.
    if (!efforts.length) {
      card.reasoningEfforts = [...card.reasoningEfforts];
      return;
    }
    // bits-ui reports a multi-select value in PRESS order (toggled-on items are
    // appended). The chips render from the card's frozen effort universe, so
    // re-sort the pressed set into that order — toggling must change pressed
    // state only, never chip order.
    const order = effortOptionsFor(card);
    card.reasoningEfforts = order.filter((e) => efforts.includes(e));
    if (!card.reasoningEfforts.includes(card.reasoningDefault)) {
      card.reasoningDefault = card.reasoningEfforts[0] ?? '';
    }
  }
  // Chip universe for a card: exactly the stored/reported levels after a
  // successful fetch, or the hardcoded default list when the fetch failed.
  // Built when the card's levels load; toggling never rebuilds it, so chips
  // don't jump.
  function effortOptionsFor(card) {
    return card.effortOptions ?? [...card.reasoningEfforts];
  }

  // Fetches metadata for ONE card from the provider's /models endpoint. The
  // server persists it immediately, so if the rest of the form has no
  // unsaved edits the dirty baseline is refreshed and Save stays disabled.
  // Returns false when the request failed (`added` only picks the toast
  // wording: a brand-new card reads "Added", a refresh reads "Fetched").
  async function fetchModelData(id, { added = false } = {}) {
    if (fetchingId || !providerReady) return false;
    const wasDirty = snapshot() !== baseline;
    fetchingId = id;
    try {
      const res = await api.setupModels([id], providerOverrides());
      const m = res?.models?.find((x) => x.model_id === id);
      const card = modelCards.find((c) => c.id === id);
      const fromProvider = res?.source?.[id] === 'provider' && !!m;
      if (card) {
        if (m) card.contextLength = String(m.context_length ?? card.contextLength);
        // The chip universe and the selected levels are exactly what was
        // reported (never padded with well-known levels), so a fetched card
        // matches what a reload shows; a failed fetch falls back to the
        // hardcoded default effort list.
        const reported = m?.reasoning_efforts ?? card.reasoningEfforts;
        card.effortOptions = [...reported];
        // Modalities mirror the provider's /models data; a failed/unknown
        // fetch falls back to text modalities.
        card.inputModality = [...(m?.input_modality ?? ['text'])];
        card.outputModality = [...(m?.output_modality ?? ['text'])];
        if (fromProvider) {
          card.reasoningEfforts = [...reported];
          card.reasoningDefault = m.reasoning_default ?? card.reasoningDefault;
        } else {
          card.reasoningEfforts = [...DEFAULT_EFFORTS];
          card.reasoningDefault = DEFAULT_EFFORT;
        }
        if (!card.reasoningEfforts.includes(card.reasoningDefault)) {
          card.reasoningDefault = card.reasoningEfforts[0] ?? '';
        }
      }
      await app.loadConfig(); // refresh chat-facing model_info (context %, efforts)
      if (!wasDirty) baseline = snapshot();
      const src = res?.source?.[id];
      app.toast(
        'success',
        src === 'provider'
          ? added ? `Added ${id}` : `Fetched data for ${id}`
          : added ? `Added ${id} with default metadata` : `No provider data for ${id} — defaults stored`,
      );
      return true;
    } catch (e) {
      app.toast('error', added ? `Could not add ${id}: ${e.message}` : `Fetch data failed: ${e.message}`);
      return false;
    } finally {
      fetchingId = '';
    }
  }

  // ---- MCP servers ----
  // Local key per row (never persisted): server rows have no stable id
  // while being edited (the name is user-editable and can be empty), and
  // index-keyed rows would mix up input focus when a middle row is deleted.
  // Shared with header rows below.
  let rowSeq = 0;
  function normalizeServer(s) {
    return {
      key: `srv-${++rowSeq}`,
      name: s.name ?? '',
      transport: s.transport ?? 'http',
      url: s.url ?? '',
      // Sorted by name so the snapshot/baseline comparison is deterministic.
      headers: Object.entries(s.headers ?? {})
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([key, value]) => ({ id: `hdr-${++rowSeq}`, key, value: value ?? '' })),
    };
  }
  // The row's headers the way the config wants them: trimmed names, blanks
  // dropped. Shared by the save patch and the "fetch tools" probe.
  function headersFor(s) {
    const headers = {};
    for (const h of s.headers ?? []) if (h.key.trim()) headers[h.key.trim()] = h.value;
    return headers;
  }
  function buildMcpServers() {
    return mcpServers
      .map((s) => ({
        name: s.name.trim(),
        transport: s.transport,
        url: s.url.trim(),
        headers: headersFor(s),
        default_enabled: true,
      }))
      .filter((s) => s.name);
  }
  function addServer() {
    mcpServers = [{ key: `srv-${++rowSeq}`, name: '', transport: 'http', url: '', headers: [] }, ...mcpServers];
  }
  function removeServer(i) {
    mcpServers = mcpServers.filter((_, idx) => idx !== i);
  }

  // ---- per-card MCP tools ----
  // The card's tool list: what its "Fetch" button last dialed, else the
  // live catalog's entries reported under the card's current name (so a saved,
  // connected server shows its toggles without a fetch). Renaming a card thus
  // empties the fallback until a fetch re-dials it or the save reconnects it
  // under the new name. Both shapes carry name + description, and the catalog
  // one keeps its default_enabled, so rows render identically.
  function toolsFor(s) {
    const name = s.name.trim();
    return serverTools[s.key] ?? (app.config?.tools ?? []).filter((t) => t.server === name);
  }

  // Dials the card's CURRENT (possibly unsaved) values and lists its tools.
  // Nothing is persisted server-side: the toggles write the same global
  // tool_defaults map the integrated list uses, and Save stores it.
  async function fetchServerTools(s) {
    const url = s.url.trim();
    if (!url || fetchingServer) return;
    fetchingServer = s.key;
    const label = s.name.trim() || url;
    try {
      const res = await api.setupMcpTools({ name: s.name.trim(), url, headers: headersFor(s) });
      const tools = res?.tools ?? [];
      serverTools = { ...serverTools, [s.key]: tools };
      app.toast('success', tools.length ? `Fetched ${tools.length} tools from ${label}` : `${label} reported no tools`);
    } catch (e) {
      app.toast('error', `Fetch failed: ${e?.message || e}`);
    } finally {
      fetchingServer = '';
    }
  }

  // ---- tool defaults ----
  // The section lists the INTEGRATED tools only; every MCP tool is toggled
  // inside its own server card instead.
  let integratedTools = $derived((app.config?.tools ?? []).filter((t) => t.server === BUILTIN_SERVER));

  function toolDefaultOn(tool) {
    // A fetched MCP row carries no default_enabled: its server default is on
    // (buildMcpServers always stores default_enabled: true).
    return tool.name in toolDefaults ? !!toolDefaults[tool.name] : (tool.default_enabled ?? true);
  }
  function setToolDefault(name, checked) {
    toolDefaults = { ...toolDefaults, [name]: checked };
  }

  // Titles stay sparse: clearing the box drops the entry so the catalog's own
  // title (the server's, or the placeholder in the box) shows again. Stored
  // as typed — the backend trims — so typing a space doesn't fight the box.
  function setToolTitle(name, value) {
    const titles = { ...toolTitles };
    if (value.trim()) titles[name] = value;
    else delete titles[name];
    toolTitles = titles;
  }

  // ---- save ----
  async function save() {
    saving = true;
    error = '';
    try {
      const patch = {
        system_prompt: systemPrompt,
        provider: { base_url: baseUrl.trim() },
        models: {
          whitelist: modelCards.map((c) => c.id),
          default_chat_model: defaultChatModel.trim(),
          default_task_model: defaultTaskModel.trim(),
          default_vision_model: defaultVisionModel.trim(),
          metas: modelCards.map((c) => ({
            model_id: c.id,
            context_length: Number(c.contextLength) || 0,
            input_modality: c.inputModality,
            output_modality: c.outputModality,
            reasoning_efforts: c.reasoningEfforts,
            reasoning_default: c.reasoningDefault,
          })),
        },
        limits: {
          upload_max_file_bytes: Number(uploadMaxBytes) || 0,
          max_tool_iterations: Number(maxToolIter) || 0,
          mcp_call_timeout_seconds: Number(mcpTimeout) || 0,
        },
      };
      // The box is preloaded with the stored key, so send it back as-is;
      // an emptied box clears the key.
      patch.provider.api_key = apiKey.trim();
      // Auth is not patchable: it is driven by the CHATTO_USERNAME /
      // CHATTO_PASSWORD server environment variables.
      patch.tool_defaults = toolDefaults;
      patch.tool_titles = toolTitles;
      // Only re-send the MCP server list when it actually changed, so
      // untouched servers don't get reconnected.
      if (JSON.stringify(mcpServers) !== baselineMcp) patch.mcp_servers = buildMcpServers();

      const data = await api.saveSetup(patch);
      applyConfig(data?.config ?? {}); // re-sync baseline; Save disables again
      await app.refreshAfterSetup();
      if (app.setupComplete === false) {
        app.toast('success', 'Saved — set the provider and default models to finish setup');
      } else {
        app.toast('success', 'Settings saved');
      }
    } catch (e) {
      error = e?.message || 'Failed to save settings';
    } finally {
      saving = false;
    }
  }

  const labelCls = 'text-sm font-medium';
  const hint = 'text-xs text-muted-foreground';
</script>

<svelte:window onkeydown={onKeydown} />

{#if visible}
  <div class="ss-anim fixed inset-0 z-[100] flex items-center justify-center p-0 sm:p-6 {closing ? 'ss-closing' : ''}">
    <!-- Backdrop: click closes only when not forced. A plain div (not a
         button) so it never competes with the real close control for focus
         or a11y selectors. -->
    <div class="absolute inset-0 cursor-default bg-black/60" role="presentation" aria-hidden="true" onclick={attemptClose}></div>

    <!-- Fullscreen on mobile (like the sidebar and panel sheets); a
         centered dialog on sm+. -->
    <div class="relative z-10 flex h-app w-full max-w-2xl flex-col bg-card text-card-foreground p-safe sm:h-auto sm:max-h-[92dvh] sm:rounded-xl sm:border sm:shadow-xl ss-panel">
      <!-- Header -->
      <div class="flex items-start justify-between gap-4 border-b px-4 py-4 sm:px-6">
        <div class="min-w-0">
          <h2 class="text-lg font-semibold">Settings</h2>
          <p class="truncate text-sm text-muted-foreground">Configure the provider, models, and server behavior</p>
        </div>
        {#if canClose}
          <button
            type="button"
            class="inline-flex size-8 shrink-0 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
            aria-label="Close settings"
            onclick={attemptClose}
          >
            <X class="size-5" strokeWidth={1.75} aria-hidden="true" />
          </button>
        {/if}
      </div>

      <!-- Forced-setup notice -->
      {#if app.setupComplete === false}
        <div class="mx-4 mt-4 rounded-lg border border-primary/30 bg-primary/10 px-4 py-3 text-sm text-foreground sm:mx-6">
          This server isn’t ready yet. Set the <strong>provider</strong> (base URL + API key) and flag a model as
          <strong>Chat</strong> and <strong>Task</strong> below, then save. You can’t close this screen until setup is complete.
        </div>
      {/if}

      <!-- Scrollable body -->
      <div class="min-h-0 flex-1 space-y-8 overflow-y-auto px-4 py-5 sm:px-6">
        {#if loading}
          <div class="flex items-center justify-center gap-2 py-12 text-sm text-muted-foreground">
            <Spinner class="size-4" /> Loading settings…
          </div>
        {:else}
          {#if error}
            <div role="alert" class="rounded-lg border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive">{error}</div>
          {/if}

          <!-- Provider -->
          <section class="space-y-3">
            <h3 class="text-base font-semibold">Provider</h3>
            <div class="space-y-1.5">
              <Label for="set-base-url" class={labelCls}>Base URL</Label>
              <Input id="set-base-url" type="text" class="h-9 font-mono text-sm" bind:value={baseUrl} placeholder="https://openrouter.ai/api/v1" autocomplete="off" spellcheck="false" />
            </div>
            <div class="space-y-1.5">
              <Label for="set-api-key" class={labelCls}>API key</Label>
              <div class="relative">
                <Input id="set-api-key" type={showApiKey ? 'text' : 'password'} class="h-9 pr-9 font-mono text-sm" bind:value={apiKey} placeholder="sk-…" autocomplete="off" />
                <button
                  type="button"
                  class="absolute inset-y-0 right-0 flex w-9 items-center justify-center text-muted-foreground transition-colors hover:text-foreground"
                  aria-label={showApiKey ? 'Hide API key' : 'Show API key'}
                  onclick={() => (showApiKey = !showApiKey)}
                >
                  {#if showApiKey}<EyeOff class="size-4" strokeWidth={1.75} aria-hidden="true" />{:else}<Eye class="size-4" strokeWidth={1.75} aria-hidden="true" />{/if}
                </button>
              </div>
            </div>
          </section>

          <!-- Models -->
          <section class="space-y-3">
            <h3 class="text-base font-semibold">Models</h3>
            <div class="space-y-1.5">
              <Label for="set-model-id" class={labelCls}>Model ID</Label>
              <div class="flex gap-2">
                <Input
                  id="set-model-id"
                  type="text"
                  class="h-9 flex-1 font-mono text-sm"
                  bind:value={newModel}
                  placeholder="e.g. openai/gpt-oss-120b"
                  autocomplete="off"
                  spellcheck="false"
                  onkeydown={(e) => e.key === 'Enter' && (e.preventDefault(), addModel())}
                />
                <Button variant="outline" size="sm" class="h-9" onclick={addModel} disabled={!providerReady || !newModel.trim() || adding}>
                  {#if adding}<Spinner class="size-3.5" />{:else}Add{/if}
                </Button>
              </div>
              {#if !providerReady}
                <p class={hint}>Enter the provider base URL and API key above to add models.</p>
              {/if}
            </div>
            {#if modelCards.length === 0}
              <p class={hint}>No models configured.</p>
            {:else}
              <div class="space-y-3">
                {#each modelCards as card (card.id)}
                  <div class="space-y-3 rounded-lg border p-3">
                    <!-- Card header: id, default flags, fetch data, delete -->
                    <div class="flex flex-wrap items-center gap-2">
                      <span class="min-w-0 flex-1 truncate font-mono text-sm">{card.id}</span>
                      <!-- Role flags: icon buttons (chat / task / vision) with tooltips. -->
                      <button
                        type="button"
                        title="Chat model"
                        aria-label="Chat model"
                        aria-pressed={defaultChatModel === card.id}
                        class="inline-flex size-7 shrink-0 items-center justify-center rounded-full border transition-colors {defaultChatModel === card.id
                          ? 'border-primary/50 bg-primary/10 text-primary'
                          : 'text-muted-foreground hover:bg-accent hover:text-foreground'}"
                        onclick={() => toggleDefaultChat(card.id)}
                      >
                        <MessageCircle class="size-3.5" strokeWidth={1.75} aria-hidden="true" />
                      </button>
                      <button
                        type="button"
                        title="Task model (background jobs like chat titles)"
                        aria-label="Task model"
                        aria-pressed={defaultTaskModel === card.id}
                        class="inline-flex size-7 shrink-0 items-center justify-center rounded-full border transition-colors {defaultTaskModel === card.id
                          ? 'border-primary/50 bg-primary/10 text-primary'
                          : 'text-muted-foreground hover:bg-accent hover:text-foreground'}"
                        onclick={() => toggleDefaultTask(card.id)}
                      >
                        <Zap class="size-3.5" strokeWidth={1.75} aria-hidden="true" />
                      </button>
                      <button
                        type="button"
                        title="Vision model (describes images for models without image input)"
                        aria-label="Vision model"
                        aria-pressed={defaultVisionModel === card.id}
                        class="inline-flex size-7 shrink-0 items-center justify-center rounded-full border transition-colors {defaultVisionModel === card.id
                          ? 'border-primary/50 bg-primary/10 text-primary'
                          : 'text-muted-foreground hover:bg-accent hover:text-foreground'}"
                        onclick={() => toggleDefaultVision(card.id)}
                      >
                        <Eye class="size-3.5" strokeWidth={1.75} aria-hidden="true" />
                      </button>
                      <Button
                        variant="outline"
                        size="sm"
                        class="h-7 gap-1 px-2 text-xs"
                        onclick={() => fetchModelData(card.id)}
                        disabled={!providerReady || fetchingId === card.id}
                      >
                        {#if fetchingId === card.id}<Spinner class="size-3" />{:else}<Download class="size-3" strokeWidth={1.75} aria-hidden="true" />{/if}
                        Fetch
                      </Button>
                      <button
                        type="button"
                        class="inline-flex size-7 shrink-0 items-center justify-center rounded-md text-muted-foreground hover:bg-destructive/10 hover:text-destructive"
                        aria-label="Remove {card.id}"
                        onclick={() => removeModel(card.id)}
                      >
                        <Trash2 class="size-4" strokeWidth={1.75} aria-hidden="true" />
                      </button>
                    </div>

                    <!-- Metadata -->
                    <div class="grid gap-3 sm:grid-cols-2">
                      <div class="space-y-1.5">
                        <Label class={labelCls}>Context window (tokens)</Label>
                        <Input type="number" min="1" class="h-8 font-mono text-xs" bind:value={card.contextLength} />
                      </div>
                      <div class="space-y-1.5">
                        <Label class={labelCls}>Default reasoning effort</Label>
                        {#if card.reasoningEfforts.length > 0}
                          <Select.Root type="single" value={card.reasoningDefault} onValueChange={(v) => (card.reasoningDefault = v)}>
                            <Select.Trigger class="h-8 w-full text-sm">
                              <span class="truncate">{card.reasoningDefault || 'select'}</span>
                            </Select.Trigger>
                            <Select.Content>
                              {#each card.reasoningEfforts as e (e)}
                                <Select.Item value={e} label={e} class="text-sm" />
                              {/each}
                            </Select.Content>
                          </Select.Root>
                        {/if}
                      </div>
                    </div>

                    <div class="grid gap-3 sm:grid-cols-2">
                      <div class="space-y-1.5">
                        <Label class={labelCls}>Input modalities</Label>
                        <ToggleGroup.Root
                          type="multiple"
                          size="sm"
                          variant="outline"
                          value={card.inputModality}
                          onValueChange={(v) => {
                            // At least one modality must stay selected: an
                            // empty change is rejected by re-pushing the
                            // current value (new reference) into the group.
                            card.inputModality = (v ?? []).length ? v : [...card.inputModality];
                          }}
                          class="w-full flex-wrap justify-start"
                        >
                          {#each MODALITIES as mod (mod)}
                            <ToggleGroup.Item value={mod}>{mod}</ToggleGroup.Item>
                          {/each}
                        </ToggleGroup.Root>
                      </div>
                      <div class="space-y-1.5">
                        <Label class={labelCls}>Output modalities</Label>
                        <ToggleGroup.Root
                          type="multiple"
                          size="sm"
                          variant="outline"
                          value={card.outputModality}
                          onValueChange={(v) => {
                            card.outputModality = (v ?? []).length ? v : [...card.outputModality];
                          }}
                          class="w-full flex-wrap justify-start"
                        >
                          {#each MODALITIES as mod (mod)}
                            <ToggleGroup.Item value={mod}>{mod}</ToggleGroup.Item>
                          {/each}
                        </ToggleGroup.Root>
                      </div>
                    </div>

                    <div class="space-y-1.5">
                      <Label class={labelCls}>Reasoning effort levels</Label>
                      <ToggleGroup.Root
                        type="multiple"
                        size="sm"
                        variant="outline"
                        value={card.reasoningEfforts}
                        onValueChange={(v) => setEfforts(card, v ?? [])}
                        class="w-full flex-wrap justify-start"
                      >
                        {#each effortOptionsFor(card) as e (e)}
                          <ToggleGroup.Item value={e}>{e}</ToggleGroup.Item>
                        {/each}
                      </ToggleGroup.Root>
                    </div>
                  </div>
                {/each}
              </div>
            {/if}
          </section>

          <!-- Auth is env-var driven (CHATTO_USERNAME / CHATTO_PASSWORD) and
               fixed at startup, so there is nothing to edit here. -->

          <!-- MCP servers -->
          <section class="space-y-3">
            <div class="flex items-center justify-between gap-2">
              <h3 class="text-base font-semibold">MCP servers</h3>
              <Button variant="outline" size="sm" class="h-9" onclick={addServer}>Add</Button>
            </div>
            {#each mcpServers as s, i (s.key)}
              <div class="space-y-2.5 rounded-lg border p-3">
                <!-- One row: Name + Transport, then Fetch + delete at the right,
                     bottom-aligned with the inputs. -->
                <div class="flex flex-wrap items-end gap-2">
                  <div class="min-w-0 flex-1 space-y-1.5">
                    <Label class={labelCls}>Name</Label>
                    <Input type="text" class="h-8 text-sm" placeholder="my-server" bind:value={s.name} />
                  </div>
                  <div class="space-y-1.5">
                    <Label class={labelCls}>Transport</Label>
                    <Select.Root type="single" value={s.transport} onValueChange={(v) => (s.transport = v)}>
                      <Select.Trigger class="h-8 w-24 shrink-0 text-sm">
                        <span>{s.transport}</span>
                      </Select.Trigger>
                      <Select.Content>
                        <Select.Item value="http" label="http" class="text-sm" />
                      </Select.Content>
                    </Select.Root>
                  </div>
                  <Button
                    variant="outline"
                    size="sm"
                    class="h-8 gap-1 px-2 text-xs"
                    onclick={() => fetchServerTools(s)}
                    disabled={!s.url.trim() || fetchingServer !== ''}
                  >
                    {#if fetchingServer === s.key}<Spinner class="size-3" />{:else}<Download class="size-3" strokeWidth={1.75} aria-hidden="true" />{/if}
                    Fetch
                  </Button>
                  <button
                    type="button"
                    class="inline-flex size-8 shrink-0 items-center justify-center rounded-md text-muted-foreground hover:bg-destructive/10 hover:text-destructive"
                    aria-label="Remove {s.name || 'server'}"
                    onclick={() => removeServer(i)}
                  >
                    <Trash2 class="size-4" strokeWidth={1.75} aria-hidden="true" />
                  </button>
                </div>
                <div class="space-y-1.5">
                  <Label class={labelCls}>Address</Label>
                  <Input type="text" class="h-8 font-mono text-xs" placeholder="https://example.com/mcp" bind:value={s.url} />
                </div>
                <div class="space-y-1.5">
                  <Label class={labelCls}>Headers</Label>
                  {#each s.headers ?? [] as h, hi (h.id)}
                    <div class="flex gap-2">
                      <Input type="text" class="h-8 flex-1 font-mono text-xs" placeholder="Header name" bind:value={h.key} />
                      <Input type="text" class="h-8 flex-1 font-mono text-xs" placeholder="Header value" bind:value={h.value} />
                      <button type="button" class="inline-flex size-8 shrink-0 items-center justify-center rounded-md text-muted-foreground hover:bg-destructive/10 hover:text-destructive" aria-label="Remove header" onclick={() => (s.headers = s.headers.filter((_, x) => x !== hi))}>
                        <X class="size-4" strokeWidth={1.75} aria-hidden="true" />
                      </button>
                    </div>
                  {/each}
                  <Button variant="outline" size="sm" class="h-9" onclick={() => (s.headers = [...(s.headers ?? []), { id: `hdr-${++rowSeq}`, key: '', value: '' }])}>Add</Button>
                </div>
                <!-- This server's tool defaults + optional user-facing titles:
                     the same global tool_defaults / tool_titles maps the
                     integrated list below writes, only scoped to the card so
                     the tools sit next to the server providing them. -->
                {#if toolsFor(s).length}
                  <div class="space-y-1.5 border-t pt-2.5">
                    <Label class={labelCls}>Tools</Label>
                    <div class="flex flex-col gap-0.5">
                      {#each toolsFor(s) as tool (tool.name)}
                        <ToolToggleRow
                          {tool}
                          checked={toolDefaultOn(tool)}
                          onToggle={(checked) => setToolDefault(tool.name, checked)}
                          titleValue={toolTitles[tool.name] ?? ''}
                          onTitle={(v) => setToolTitle(tool.name, v)}
                        />
                      {/each}
                    </div>
                  </div>
                {:else if s.url.trim()}
                  <p class={hint}>No tools listed — press Fetch to dial this server.</p>
                {/if}
              </div>
            {:else}
              <p class={hint}>No MCP servers configured.</p>
            {/each}
          </section>

          <!-- Integrated tool defaults (MCP tools live in their server cards) -->
          <section class="space-y-3">
            <h3 class="text-base font-semibold">Tool defaults</h3>
            <p class={hint}>What each new chat starts with. The per-chat Tools menu overrides these for that chat only. MCP tools are toggled in their own server card above.</p>
            <div class="flex flex-col gap-0.5">
              {#each integratedTools as tool (tool.name)}
                <ToolToggleRow {tool} checked={toolDefaultOn(tool)} onToggle={(checked) => setToolDefault(tool.name, checked)} />
              {:else}
                <p class={hint}>No integrated tools in the catalog yet.</p>
              {/each}
            </div>
          </section>

          <!-- System prompt -->
          <section class="space-y-3">
            <h3 class="text-base font-semibold">System prompt</h3>
            <textarea
              class="min-h-40 w-full rounded-md border bg-background p-3 font-mono text-xs leading-relaxed focus:outline-none focus:ring-2 focus:ring-ring"
              bind:value={systemPrompt}
              spellcheck="false"
            ></textarea>
          </section>

          <!-- Limits -->
          <section class="space-y-3">
            <h3 class="text-base font-semibold">Limits</h3>
            <div class="grid gap-3 sm:grid-cols-3">
              <div class="space-y-1.5">
                <Label for="set-upload" class={labelCls}>Max upload (bytes)</Label>
                <Input id="set-upload" type="number" min="0" class="h-9 text-sm" bind:value={uploadMaxBytes} />
              </div>
              <div class="space-y-1.5">
                <Label for="set-tool-iter" class={labelCls}>Max MCP tool calls per response</Label>
                <Input id="set-tool-iter" type="number" min="0" class="h-9 text-sm" bind:value={maxToolIter} />
              </div>
              <div class="space-y-1.5">
                <Label for="set-mcp-timeout" class={labelCls}>MCP call timeout (s)</Label>
                <Input id="set-mcp-timeout" type="number" min="0" class="h-9 text-sm" bind:value={mcpTimeout} />
              </div>
            </div>
          </section>
        {/if}
      </div>

      <!-- Footer -->
      <div class="flex items-center justify-end gap-2 border-t px-4 py-4 sm:px-6">
        {#if canClose}
          <Button variant="outline" onclick={attemptClose} disabled={saving}>Cancel</Button>
        {/if}
        <Button onclick={save} disabled={saving || loading || !dirty}>
          {#if saving}<Spinner class="size-4" light label="Saving" />{/if}
          Save changes
        </Button>
      </div>
    </div>
  </div>
{/if}

<style>
  /* Open/close animation, class-driven like the lightbox's: the wrapper fades
     while the panel zooms 0.96<->1. Exit holds via .ss-closing until the
     `visible` state drops the block. */
  .ss-anim { animation: ss-fade-in 180ms ease-out; }
  .ss-anim > .ss-panel { animation: ss-zoom-in 180ms ease-out; }
  .ss-closing { animation: ss-fade-out 150ms ease-in forwards; }
  .ss-closing > .ss-panel { animation: ss-zoom-out 150ms ease-in forwards; }
  @keyframes ss-fade-in {
    from { opacity: 0; }
  }
  @keyframes ss-fade-out {
    to { opacity: 0; }
  }
  @keyframes ss-zoom-in {
    from { transform: scale(0.96); }
  }
  @keyframes ss-zoom-out {
    to { transform: scale(0.96); }
  }
  @media (prefers-reduced-motion: reduce) {
    .ss-anim,
    .ss-anim > .ss-panel,
    .ss-closing,
    .ss-closing > .ss-panel { animation: none; }
  }
</style>
