<script>
  import { app, ACCEPT_FILE_EXTS } from '../lib/state.svelte.js';
  import { onMount } from 'svelte';
  import { EyeOff, Paperclip, SendHorizontal, Square, X } from '@lucide/svelte';
  import Spinner from './Spinner.svelte';
  import { Button } from '$lib/components/ui/button';
  import * as Select from '$lib/components/ui/select';
  import * as Tooltip from '$lib/components/ui/tooltip';
  import ModelPickerSheet from './ModelPickerSheet.svelte';
  import AttachmentSheet from './AttachmentSheet.svelte';
  import { isNative } from '../lib/server.js';
  import { capturePhoto, pickPhotos, pickFiles } from '../lib/native-attachments.js';

  // Draft text lives in the store (keyed by chat id) so it survives the
  // Composer remount that ensureChat() triggers on the first send — and
  // chat switches. Same for pending attachments, which are staged client-side
  // (File objects) and only uploaded when the message is actually sent.
  let pending = $derived(app.pendingList());
  let sending = $state(false);
  let fileInput = $state(null);
  let textArea = $state(null);
  // Native (Capacitor): the attach button opens a bottom drawer with
  // camera/photos/files shortcuts instead of the plain file input.
  let attachOpen = $state(false);
  let attachBusy = $state(false);

  // New (not-yet-created) chat: focus the prompt on mount so the mobile
  // soft keyboard pops up and desktop users can type right away.
  onMount(() => {
    if (app.activeChatId == null) textArea?.focus();
  });

  // Model selection lives in the composer (ChatGPT/Gemini style). For an
  // existing chat it patches the chat's model; for a not-yet-created chat it
  // sets the draft model used at creation.
  let models = $derived(app.config?.models?.whitelist ?? []);
  let currentModel = $derived(app.chat ? (app.chat.model ?? '') : app.newChatModel);
  let modelLabel = $derived(currentModel || 'Select model');

  // Reasoning effort: per-model levels from the provider's /models endpoint
  // (via /api/config model_info), shown lowest -> highest intensity.
  const EFFORT_RANK = { none: 0, minimal: 1, low: 2, medium: 3, high: 4, xhigh: 5, max: 6 };
  let effortOptions = $derived(
    [...app.effortOptionsFor(currentModel)].sort(
      (a, b) => (EFFORT_RANK[a] ?? 99) - (EFFORT_RANK[b] ?? 99),
    ),
  );
  // Effective selection: explicit choice when valid for the model, else the
  // default ("medium" when supported). Invalid selections (e.g. after a
  // model switch) fall back to the default rather than showing a stale value.
  let currentEffort = $derived.by(() => {
    const chosen = app.chat ? app.chat.params?.reasoning_effort : app.newChatEffort;
    const opts = app.effortOptionsFor(currentModel);
    if (chosen && opts.includes(chosen)) return chosen;
    return app.defaultEffortFor(currentModel);
  });

  // Touch devices have no hover, so the image caveat can't surface as a
  // tooltip there — tapping the icon expands the full text inline instead
  // (a toast proved unstable under touch: it renders near the finger and
  // sonner's swipe-dismiss kills it).
  const canHover = matchMedia('(hover: hover)').matches;
  let hintOpen = $state(false);

  // Vision-model hint: warn when staged images target a chat model that
  // can't see them — the server substitutes a vision-model description in
  // that case (no images travel to the chat model).
  let currentInputModality = $derived(
    app.modelInfo.find((m) => m.id === currentModel)?.input_modality ?? [],
  );
  let hasStagedImages = $derived(pending.some((a) => a.kind === 'image'));
  let imageHint = $derived.by(() => {
    if (!hasStagedImages) return '';
    // Models without stored metadata default to text-only server-side.
    if (currentInputModality.includes('image')) return '';
    const visionModel = app.config?.models?.default_vision_model ?? '';
    return visionModel
      ? `This model can't see images — they'll be described by ${visionModel}.`
      : "This model can't see images, and no vision model is configured.";
  });

  function handleModelChange(value) {
    if (!value) return;
    if (app.chat) {
      const patch = { model: value };
      // Reconcile a persisted effort the new model doesn't support.
      const cur = app.chat.params?.reasoning_effort;
      if (cur && !app.effortOptionsFor(value).includes(cur)) {
        patch.params = { ...(app.chat.params ?? {}) };
        const def = app.defaultEffortFor(value);
        if (def) patch.params.reasoning_effort = def;
        else delete patch.params.reasoning_effort;
      }
      app.patchChat(patch);
    } else {
      app.newChatModel = value;
      if (app.newChatEffort && !app.effortOptionsFor(value).includes(app.newChatEffort)) {
        app.newChatEffort = '';
      }
    }
  }

  function handleEffortChange(value) {
    if (!value) return;
    if (app.chat) {
      app.patchChat({ params: { ...(app.chat.params ?? {}), reasoning_effort: value } });
    } else {
      app.newChatEffort = value;
    }
  }

  // accept="" attribute mirrors the attachable-extension lists (single
  // source of truth in state.svelte.js).
  const ACCEPT = ACCEPT_FILE_EXTS.map((e) => `.${e}`).join(',');

  let canSend = $derived(
    (app.draftFor().trim().length > 0 || pending.length > 0) && !sending,
  );

  function autoGrow() {
    if (!textArea) return;
    textArea.style.height = 'auto';
    textArea.style.height = `${Math.min(textArea.scrollHeight, 240)}px`;
  }

  // Grow to fit a restored draft (store-backed drafts survive remounts and
  // chat switches, so the initial value may be multi-line).
  $effect(() => {
    app.draftFor(); // dependency
    requestAnimationFrame(autoGrow);
  });

  function onKeydown(e) {
    // Touch devices (mobile web + native app) have no Shift+Enter, so plain
    // Enter must insert a newline there; sending happens via the send button.
    // Checked live per keydown so docking/undocking keyboards on hybrids is
    // picked up without a remount.
    const touchOnly = matchMedia('(pointer: coarse)').matches;
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing && !touchOnly) {
      e.preventDefault();
      if (canSend) send();
    }
  }

  async function send() {
    if (!canSend) return;
    const content = app.draftFor().trim();
    // Snapshot BEFORE clearing: staged File objects survive the Composer
    // remount that ensureChat() triggers on the first send.
    const staged = pending;
    app.setDraft('');
    textArea?.focus();
    requestAnimationFrame(autoGrow);
    app.clearPendingAttachments();
    sending = true;
    try {
      await app.send(content, staged);
    } catch {
      // toast already shown by state; restore draft + attachments so the
      // user doesn't lose them. NOTE: ensureChat() may have remounted this
      // Composer — the store-backed draft lands in the NEW instance.
      app.setDraft(content);
      for (const a of staged) app.restorePendingAttachment(a);
      requestAnimationFrame(autoGrow);
    } finally {
      sending = false;
    }
  }

  // Clipboard paste: images (and files) land as clipboardData.files, not
  // as insertable text — upload them as pending attachments instead of
  // letting the paste fall through or drop binary gibberish into the draft.
  // Staging is client-side only (no network), so both handlers are plain.
  function onPaste(e) {
    const files = Array.from(e.clipboardData?.files ?? []);
    if (files.length === 0) return; // plain text paste: default behavior
    e.preventDefault();
    app.addAttachments(files);
  }

  function onFiles(e) {
    const files = Array.from(e.target.files ?? []);
    e.target.value = '';
    if (files.length === 0) return;
    app.addAttachments(files);
  }

  function onAttachButton(e) {
    // Blur before the native file dialog opens: without this Firefox/Chrome
    // keep the button :focus-visible (and touch emulators leave :hover
    // stuck) once the dialog closes, making hover look broken afterwards.
    e?.currentTarget?.blur();
    if (isNative()) attachOpen = true;
    else fileInput?.click();
  }

  // A drawer row was tapped: close the drawer right away (the native picker
  // covers the screen), then stage whatever comes back. Cancellation yields
  // [] and is silent; real failures toast.
  async function handleAttachAction(kind) {
    attachOpen = false;
    attachBusy = true;
    try {
      let files = [];
      if (kind === 'camera') files = await capturePhoto();
      else if (kind === 'photos') files = await pickPhotos();
      else files = await pickFiles();
      if (files.length) app.addAttachments(files);
    } catch (err) {
      app.toast('error', `Couldn't attach: ${err?.message ?? 'unknown error'}`);
    } finally {
      attachBusy = false;
    }
  }

  function removePending(id) {
    app.removePendingAttachment(id);
  }
</script>

<div class="shrink-0 px-3 pb-4 pt-2 sm:px-6">
  <div class="mx-auto max-w-4xl">
    <div class="rounded-2xl border bg-card shadow-sm transition-colors focus-within:border-ring/50">
      {#if pending.length > 0}
        <div class="flex flex-wrap items-center gap-1.5 px-3 pt-3">
          {#each pending as att (att.id)}
            <span class="inline-flex items-center gap-1.5 rounded-full bg-accent px-2.5 py-1 text-xs">
              {#if att.kind === 'image' && att.previewUrl}
                <img src={att.previewUrl} alt={att.filename} class="size-5 rounded object-cover" />
              {:else}
                <Paperclip class="size-3" strokeWidth={1.75} aria-hidden="true" />
              {/if}
              <span class="max-w-40 truncate">{att.filename}</span>
              <button
                class="flex size-4 items-center justify-center rounded-full transition-colors hover:bg-foreground/10"
                title="Remove attachment"
                onclick={() => removePending(att.id)}
              >
                <X class="size-3" strokeWidth={1.75} aria-hidden="true" />
              </button>
            </span>
          {/each}
          {#if imageHint}
            <!-- Compact caveat affordance: hover shows the full text as a
                 tooltip (desktop); on touch the tooltip is disabled and a
                 tap toggles the text inline below the chips instead. -->
            <Tooltip.Root disabled={!canHover}>
              <Tooltip.Trigger
                class="inline-flex size-5 shrink-0 items-center justify-center rounded-full text-destructive transition-colors hover:bg-destructive/10"
                aria-label={imageHint}
                aria-expanded={!canHover ? hintOpen : undefined}
                onclick={() => {
                  if (!canHover) hintOpen = !hintOpen;
                }}
              >
                <EyeOff class="size-3.5" strokeWidth={1.75} aria-hidden="true" />
              </Tooltip.Trigger>
              <Tooltip.Content side="top" class="max-w-64">{imageHint}</Tooltip.Content>
            </Tooltip.Root>
          {/if}
        </div>
        {#if imageHint && hintOpen}
          <p class="px-3 pt-1.5 text-xs text-muted-foreground">{imageHint}</p>
        {/if}
      {/if}

      <div class="flex items-end gap-1.5 p-2.5">
        <Button
          variant="ghost"
          size="icon"
          class="size-9 shrink-0 rounded-full"
          title="Attach files (images: jpg/png/gif/webp, text/code files)"
          onclick={onAttachButton}
          disabled={attachBusy}
        >
          {#if attachBusy}
            <Spinner class="size-4" label="Attaching" />
          {:else}
            <Paperclip class="size-[18px]" strokeWidth={1.75} aria-hidden="true" />
          {/if}
        </Button>
        <input bind:this={fileInput} type="file" class="hidden" multiple accept={ACCEPT} onchange={onFiles} />

        <textarea
          bind:this={textArea}
          bind:value={() => app.draftFor(), (v) => app.setDraft(v)}
          class="max-h-60 min-h-9 min-w-0 flex-1 resize-none bg-transparent px-1 py-1.5 text-[0.9375rem] leading-6 outline-none placeholder:text-muted-foreground"
          rows="1"
          placeholder="Message…"
          oninput={autoGrow}
          onkeydown={onKeydown}
          onpaste={onPaste}
        ></textarea>

        {#if app.generating}
          <Button
            variant="outline"
            size="icon"
            class="size-9 shrink-0 rounded-full border-destructive/50 text-destructive hover:bg-destructive/10"
            title="Stop generation"
            onclick={() => app.stopGeneration()}
          >
            <Square class="size-4" strokeWidth={1.75} aria-hidden="true" />
          </Button>
        {:else}
          <Button
            size="icon"
            class="size-9 shrink-0 rounded-full"
            title="Send"
            onclick={send}
            disabled={!canSend}
          >
            {#if sending}
              <Spinner class="size-4" label="Sending" />
            {:else}
              <SendHorizontal class="size-4" strokeWidth={1.75} aria-hidden="true" />
            {/if}
          </Button>
        {/if}
      </div>

      <!-- Bottom bar: model + reasoning-effort pickers, keyboard hint -->
      <div class="flex items-center justify-between gap-2 border-t border-border/60 px-2 py-1">
        <!-- Desktop: popover selects. Below sm: ModelPickerSheet (bottom drawer). -->
        <div class="hidden min-w-0 items-center gap-1 sm:flex">
          <Select.Root
            type="single"
            value={currentModel}
            onValueChange={handleModelChange}
            disabled={!app.config}
          >
            <Select.Trigger
              class="h-8 w-auto max-w-56 overflow-hidden border-transparent bg-transparent px-2 font-mono text-xs text-muted-foreground shadow-none hover:bg-accent hover:text-accent-foreground dark:bg-transparent dark:hover:bg-accent"
              title={modelLabel}
            >
              <span class="min-w-0 truncate">{modelLabel}</span>
            </Select.Trigger>
            <Select.Content side="top" align="start">
              {#each models as m (m)}
                <Select.Item value={m} label={m} class="font-mono text-xs" />
              {/each}
            </Select.Content>
          </Select.Root>

          {#if effortOptions.length > 0}
            <Select.Root type="single" value={currentEffort} onValueChange={handleEffortChange}>
              <Select.Trigger
                class="h-8 w-auto border-transparent bg-transparent px-2 font-mono text-xs text-muted-foreground shadow-none hover:bg-accent hover:text-accent-foreground dark:bg-transparent dark:hover:bg-accent"
                title="Reasoning effort for the next response"
              >
                {currentEffort}
              </Select.Trigger>
              <Select.Content side="top" align="start">
                {#each effortOptions as e (e)}
                  <Select.Item value={e} label={e} class="font-mono text-xs" />
                {/each}
              </Select.Content>
            </Select.Root>
          {/if}
        </div>

        <div class="flex min-w-0 flex-1 items-center justify-center sm:hidden">
          <ModelPickerSheet
            {models}
            currentModel={currentModel}
            currentEffort={currentEffort}
            {effortOptions}
            onModelChange={handleModelChange}
            onEffortChange={handleEffortChange}
          />
        </div>

        <span class="hidden text-[11px] text-muted-foreground/60 sm:inline">
          Enter to send · Shift+Enter for newline
        </span>
      </div>
    </div>
  </div>
</div>

<!-- Native-only: camera / photos / files shortcuts (plain web keeps the
     hidden file input above). -->
<AttachmentSheet bind:open={attachOpen} busy={attachBusy} onAction={handleAttachAction} />
