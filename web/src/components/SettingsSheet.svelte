<script>
  import { app } from '../lib/state.svelte.js';
  import { api } from '../lib/api.js';
  import { Download, Eye, EyeOff, MessageCircle, Trash2, X, Zap } from '@lucide/svelte';
  import Spinner from './Spinner.svelte';
  import { Button } from '$lib/components/ui/button';
  import { Input } from '$lib/components/ui/input';
  import { Label } from '$lib/components/ui/label';
  import * as Select from '$lib/components/ui/select';
  import { Switch } from '$lib/components/ui/switch';
  import * as ToggleGroup from '$lib/components/ui/toggle-group';
  import { registerOverlay } from '../lib/overlays.svelte.js';

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
  // MCP server names as of the last loaded config: the rows above are these
  // plus the user's unsaved edits, and the difference drives the tool list.
  let loadedServers = $state([]);
  // Global per-tool default toggles: sparse map (tool name → bool) of the
  // explicit overrides. Rows render from the live catalog (app.config.tools);
  // a tool absent from the map keeps its own default_enabled.
  let toolDefaults = $state({});

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

  const MODALITIES = ['text', 'image', 'audio', 'video'];
  const DEFAULT_EFFORTS = ['low', 'medium', 'high'];
  const DEFAULT_CONTEXT = 131072;

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
        reasoningDefault: m.reasoning_default ?? DEFAULT_EFFORTS[1],
      };
    });
    mcpServers = (c.mcp_servers ?? []).map(normalizeServer);
    loadedServers = (c.mcp_servers ?? []).map((s) => s.name ?? '');
    toolDefaults = { ...(c.tool_defaults ?? {}) };
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
  // Adds a whitelisted model: fetches (and server-side persists) its
  // metadata from the provider BEFORE the card appears, so the card opens
  // populated with real data. Unconfigured provider or an unknown model id
  // falls back to defaults; a failed request adds nothing.
  async function addModel() {
    const id = newModel.trim();
    if (!id || adding || !providerReady) return;
    if (modelCards.some((c) => c.id === id)) {
      app.toast('error', `${id} is already whitelisted`);
      return;
    }
    adding = true;
    try {
      const res = await api.setupModels([id], providerOverrides());
      const m = res?.models?.find((x) => x.id === id);
      const fromProvider = res?.source?.[id] === 'provider' && !!m;
      const reported = m?.reasoning_efforts ?? DEFAULT_EFFORTS;
      // Modalities mirror what the provider's /models reported (the backend
      // already falls back per field to defaults); a failed/unknown fetch
      // falls back to the hardcoded default effort list. The chip universe
      // is exactly the reported/default list (never padded with well-known
      // levels), so a freshly added card matches what a reload would show.
      modelCards = [
        {
          id,
          contextLength: String(m?.context_window ?? DEFAULT_CONTEXT),
          inputModality: [...(m?.input_modality ?? ['text'])],
          outputModality: [...(m?.output_modality ?? ['text'])],
          reasoningEfforts: [...reported],
          effortOptions: [...reported],
          reasoningDefault: fromProvider ? (m.reasoning_default ?? DEFAULT_EFFORTS[1]) : DEFAULT_EFFORTS[1],
        },
        ...modelCards,
      ];
      newModel = '';
      await app.loadConfig(); // refresh chat-facing model_info
      const src = res?.source?.[id];
      app.toast('success', src === 'provider' ? `Added ${id}` : `Added ${id} with default metadata`);
    } catch (e) {
      app.toast('error', `Could not add ${id}: ${e.message}`);
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
  async function fetchModelData(id) {
    if (fetchingId || !providerReady) return;
    const wasDirty = snapshot() !== baseline;
    fetchingId = id;
    try {
      const res = await api.setupModels([id], providerOverrides());
      const m = res?.models?.find((x) => x.id === id);
      const card = modelCards.find((c) => c.id === id);
      const fromProvider = res?.source?.[id] === 'provider' && !!m;
      if (card) {
        if (m) card.contextLength = String(m.context_window ?? card.contextLength);
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
          card.reasoningDefault = DEFAULT_EFFORTS[1];
        }
        if (!card.reasoningEfforts.includes(card.reasoningDefault)) {
          card.reasoningDefault = card.reasoningEfforts[0] ?? '';
        }
      }
      await app.loadConfig(); // refresh chat-facing model_info (context %, efforts)
      if (!wasDirty) baseline = snapshot();
      const src = res?.source?.[id];
      app.toast('success', src === 'provider' ? `Fetched data for ${id}` : `No provider data for ${id} — defaults stored`);
    } catch (e) {
      app.toast('error', `Fetch data failed: ${e.message}`);
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
      command: s.command ?? '',
      argsText: (s.args ?? []).join(' '),
      url: s.url ?? '',
      // Sorted by name so the snapshot/baseline comparison is deterministic.
      headers: Object.entries(s.headers ?? {})
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([key, value]) => ({ id: `hdr-${++rowSeq}`, key, value: value ?? '' })),
    };
  }
  function buildMcpServers() {
    return mcpServers
      .map((s) => {
        const out = { name: s.name.trim(), transport: s.transport, default_enabled: true };
        if (s.transport === 'stdio') {
          out.command = s.command.trim();
          out.args = s.argsText.split(/\s+/).filter(Boolean);
        } else {
          out.url = s.url.trim();
          const headers = {};
          for (const h of s.headers ?? []) if (h.key.trim()) headers[h.key.trim()] = h.value;
          out.headers = headers;
        }
        return out;
      })
      .filter((s) => s.name);
  }
  function addServer() {
    mcpServers = [{ key: `srv-${++rowSeq}`, name: '', transport: 'http', command: '', argsText: '', url: '', headers: [] }, ...mcpServers];
  }
  function removeServer(i) {
    mcpServers = mcpServers.filter((_, idx) => idx !== i);
  }

  // ---- global tool defaults ----
  // Rows for the section: the live catalog minus the tools of any MCP server
  // whose row was removed or renamed in the form, so the list follows the
  // edits immediately instead of waiting for the save + catalog refetch.
  // (A newly added server's tools can only appear after saving — nothing
  // knows its tool names until the server is dialed.)
  let settingsTools = $derived.by(() => {
    const live = new Set(mcpServers.map((s) => s.name.trim()));
    const gone = new Set(loadedServers.filter((n) => n && !live.has(n)));
    return (app.config?.tools ?? []).filter((t) => !gone.has(t.server));
  });

  function toolDefaultOn(tool) {
    return tool.name in toolDefaults ? !!toolDefaults[tool.name] : !!tool.default_enabled;
  }
  function setToolDefault(name, checked) {
    toolDefaults = { ...toolDefaults, [name]: checked };
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

{#if open}
  <div class="fixed inset-0 z-[100] flex items-center justify-center p-0 sm:p-6">
    <!-- Backdrop: click closes only when not forced. A plain div (not a
         button) so it never competes with the real close control for focus
         or a11y selectors. -->
    <div class="absolute inset-0 cursor-default bg-black/60" role="presentation" aria-hidden="true" onclick={attemptClose}></div>

    <!-- Fullscreen on mobile (like the sidebar and panel sheets); a
         centered dialog on sm+. -->
    <div class="relative z-10 flex h-dvh w-full max-w-2xl flex-col bg-card text-card-foreground p-safe sm:h-auto sm:max-h-[92dvh] sm:rounded-xl sm:border sm:shadow-xl">
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
                <div class="grid gap-2.5 sm:grid-cols-2">
                  <div class="space-y-1.5">
                    <Label class={labelCls}>Name</Label>
                    <Input type="text" class="h-8 text-sm" placeholder="my-server" bind:value={s.name} />
                  </div>
                  <div class="space-y-1.5">
                    <Label class={labelCls}>Transport</Label>
                    <div class="flex gap-2">
                      <Select.Root type="single" value={s.transport} onValueChange={(v) => (s.transport = v)}>
                        <Select.Trigger class="h-8 w-24 shrink-0 text-sm">
                          <span>{s.transport}</span>
                        </Select.Trigger>
                        <Select.Content>
                          <Select.Item value="http" label="http" class="text-sm" />
                          <Select.Item value="stdio" label="stdio" class="text-sm" />
                        </Select.Content>
                      </Select.Root>
                      <button type="button" class="inline-flex size-8 shrink-0 items-center justify-center rounded-md text-muted-foreground hover:bg-destructive/10 hover:text-destructive" aria-label="Remove server" onclick={() => removeServer(i)}>
                        <Trash2 class="size-4" strokeWidth={1.75} aria-hidden="true" />
                      </button>
                    </div>
                  </div>
                </div>
                {#if s.transport === 'stdio'}
                  <div class="grid gap-2.5 sm:grid-cols-2">
                    <div class="space-y-1.5">
                      <Label class={labelCls}>Command</Label>
                      <Input type="text" class="h-8 font-mono text-xs" placeholder="e.g. npx" bind:value={s.command} />
                    </div>
                    <div class="space-y-1.5">
                      <Label class={labelCls}>Arguments</Label>
                      <Input type="text" class="h-8 font-mono text-xs" placeholder="space-separated" bind:value={s.argsText} />
                    </div>
                  </div>
                {:else}
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
                {/if}
              </div>
            {:else}
              <p class={hint}>No MCP servers configured.</p>
            {/each}
          </section>

          <!-- Global tool defaults -->
          <section class="space-y-3">
            <h3 class="text-base font-semibold">Tool defaults</h3>
            <p class={hint}>What each new chat starts with. The per-chat Tools menu overrides these for that chat only. Tools of a server you just added appear after saving.</p>
            <div class="flex flex-col gap-0.5">
              {#each settingsTools as tool (tool.name)}
                <div class="flex items-center gap-3 rounded-md p-2 transition-colors hover:bg-accent/50">
                  <div class="flex shrink-0 items-center">
                    <Switch checked={toolDefaultOn(tool)} aria-label="{tool.name} enabled by default" onCheckedChange={(checked) => setToolDefault(tool.name, checked)} />
                  </div>
                  <div class="min-w-0">
                    <div class="truncate text-sm leading-5 font-medium">{tool.name}</div>
                    <div class="line-clamp-2 text-xs text-muted-foreground">{tool.description || tool.server}</div>
                  </div>
                </div>
              {:else}
                <p class={hint}>No tools in the catalog yet.</p>
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
                <Label for="set-tool-iter" class={labelCls}>Max tool iterations</Label>
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
