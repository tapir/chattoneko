<script>
  import { app } from '../lib/state.svelte.js';
  import {
    renderMarkdown,
    stablePrefixLen,
    closeOpenInline,
    escapeHtml,
    isMarkdownReady,
    onMarkdownReady,
  } from '../lib/markdown.js';
  import { formatDuration, formatTokensOrDash } from '../lib/format.js';
  import { copyText } from '../lib/clipboard.js';
  import { api } from '../lib/api.js';
  import { viewer } from '../lib/viewer.svelte.js';
  import { Eye, FileText, Info, Paperclip, Pencil, RotateCcw, X } from '@lucide/svelte';
  import ThinkingBlock from './ThinkingBlock.svelte';
  import Scramble from './Scramble.svelte';
  import ImageGallery from './ImageGallery.svelte';
  import ToolCallItem from './ToolCallItem.svelte';
  import GenerationError from './GenerationError.svelte';
  import IconButton from './IconButton.svelte';
  import CopyButton from './CopyButton.svelte';
  import Spinner from './Spinner.svelte';
  import { Button } from '$lib/components/ui/button';
  import * as Popover from '$lib/components/ui/popover';

  let { item, isLive = false, isLastAssistant = false, track = null } = $props();

  let msg = $derived(item.msg);
  let live = $derived(isLive ? app.live : null);
  // Model that produced this message; messages predating per-message model
  // tracking fall back to the chat's current model.
  let msgModel = $derived(msg.model || app.chat?.model || '');

  // Live messages render purely from stream events (B4); terminal messages
  // render from REST-persisted fields.
  let content = $derived(live ? live.display : msg.content);
  let reasoning = $derived(live ? live.reasoningDisplay : msg.reasoning);
  let toolCalls = $derived(live ? live.toolCalls : item.toolCalls);
  let status = $derived(live ? live.status : msg.status);
  let errorText = $derived(live ? live.error : msg.error);
  // Tool-created attachments (create_text_file, show_image) on this
  // assistant message. Deliberately NOT rendered while the message is live:
  // they appear only once the reply is fully rendered (`done` merges
  // live.attachments into the message, which ends `live`), instead of
  // popping in mid-stream above text that is still typing.
  let attachments = $derived(live ? [] : (msg.attachments ?? []));
  // Image attachments render inline (show_image exists so the user SEES the
  // picture); everything else keeps the download-chip treatment.
  let imageFiles = $derived(attachments.filter((a) => a.kind === 'image'));
  let files = $derived(attachments.filter((a) => a.kind !== 'image'));
  // User-message attachments, split the same way for the non-editing view:
  // images go to the gallery, everything else keeps the chip row.
  let userImageFiles = $derived((msg.attachments ?? []).filter((a) => a.kind === 'image'));
  let userFiles = $derived((msg.attachments ?? []).filter((a) => a.kind !== 'image'));

  // ---- streaming markdown: stable prefix + unfinished tail ----
  // The typewriter grows `content` a few chars per animation frame. Parsing
  // the WHOLE document with marked + highlight.js + DOMPurify on every frame
  // is what made streaming stutter, so only complete lines (the stable
  // prefix) go through the markdown pipeline, at most every PROMOTE_MS. The
  // tail is a single unfinished line, so it is cheap to parse per frame once
  // its open syntax is closed (tailHtml below) — no literal `**` mid-line.
  const PROMOTE_MS = 90;
  let stableText = $state('');
  // The prefix ended inside an open ``` fence: the tail is a code line and
  // must stay literal, or markdown would eat its #, * and backticks.
  let tailCode = $state(false);
  let lastPromote = 0;
  let promoteTimer = null;

  function promoteNow() {
    const c = content;
    if (!c.startsWith(stableText)) stableText = '';
    const cut = stablePrefixLen(c);
    if (cut > stableText.length) stableText = c.slice(0, cut);
    tailCode = ((stableText.match(/^\s*```/gm) ?? []).length) % 2 === 1;
    lastPromote = Date.now();
  }

  $effect(() => {
    const c = content;
    if (!isLive) {
      // Terminal/historical message: render everything in one pass.
      clearTimeout(promoteTimer);
      promoteTimer = null;
      if (stableText !== c) stableText = c;
      return;
    }
    // stream was reset
    if (!c.startsWith(stableText)) {
      stableText = '';
      tailCode = false;
    }
    if (stablePrefixLen(c) <= stableText.length) return;
    const elapsed = Date.now() - lastPromote;
    if (elapsed >= PROMOTE_MS) {
      clearTimeout(promoteTimer);
      promoteTimer = null;
      promoteNow();
    } else if (promoteTimer == null) {
      promoteTimer = setTimeout(() => {
        promoteTimer = null;
        if (isLive) promoteNow();
      }, PROMOTE_MS - elapsed);
    }
  });

  // Drop a pending promotion when the component unmounts (chat switch while
  // streaming); the effect above re-arms it on every mount/content change.
  $effect(() => () => clearTimeout(promoteTimer));

  let tail = $derived(isLive ? content.slice(stableText.length) : '');

  // The markdown pipeline (marked + Temml + highlight.js + DOMPurify) is a
  // lazily-loaded chunk, so it may not have arrived when this first renders.
  // renderMarkdown falls back to escaped plain text until it does; flipping
  // this flag is what makes `html` below re-run and swap in real markup.
  // Without the subscription a TERMINAL message would stay stuck on the
  // fallback forever — nothing else about it changes after mount.
  let mdReady = $state(isMarkdownReady());
  $effect(() => onMarkdownReady(() => (mdReady = true)));

  // Single markdown parse per promotion while live (never per frame);
  // terminal messages parse once, synchronously, from their full content.
  let html = $derived.by(() => {
    void mdReady; // dependency: re-run when the lazy chunk lands
    if (msg.role !== 'assistant') return '';
    return renderMarkdown(isLive ? stableText : content);
  });

  // The tail rendered as real markdown: close its open syntax and parse it —
  // one unfinished line, so per-frame is cheap — then unwrap the <p>. The tail
  // continues the prefix's last block, and a real paragraph would add block
  // margins that collapse again at the next promotion. null from
  // closeOpenInline (fence line, unclosed display math) and a prefix that ended
  // inside a code fence keep the literal escaped span.
  let tailHtml = $derived.by(() => {
    void mdReady; // dependency: re-run when the lazy chunk lands
    if (!tail) return '';
    const closed = tailCode ? null : closeOpenInline(tail);
    if (closed === null)
      return `<span class="whitespace-pre-wrap">${escapeHtml(tail)}</span>`;
    const md = renderMarkdown(closed);
    const p = /^<p>([\s\S]*)<\/p>\s*$/.exec(md);
    return p ? p[1] : md;
  });

  // Post-render decoration: per-code-block copy button.
  let contentEl = $state(null);
  $effect(() => {
    html; // re-run when rendered markdown changes
    if (!contentEl) return;
    for (const pre of contentEl.querySelectorAll('pre.codeblock')) {
      if (pre.querySelector(':scope > .codeblock-head')) continue;
      const head = document.createElement('div');
      head.className = 'codeblock-head';
      const lang = document.createElement('span');
      lang.textContent = pre.dataset.lang || 'text';
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'codeblock-copy';
      btn.textContent = 'copy';
      btn.onclick = () => {
        const code = pre.querySelector('code');
        copyText(code ? code.innerText : pre.innerText).then((ok) => {
          btn.textContent = ok ? 'copied' : 'failed';
          setTimeout(() => (btn.textContent = 'copy'), 1200);
        });
      };
      head.append(lang, btn);
      pre.prepend(head);
    }
  });

  // Notify parent (MessageList) whenever the rendered content changes,
  // so it can follow the stream to the bottom. content/reasoning/toolCalls
  // are genuinely accessed here -> dependency tracking is explicit.
  $effect(() => {
    track?.(content, reasoning, toolCalls);
  });

  // ---- inline edit (user messages) ----
  let editing = $state(false);
  let editValue = $state('');
  let editArea = $state(null);
  // Attachments kept by the edit; initialized from the message and pruned
  // via the remove buttons. Only sent to the API when it differs.
  let editAttachments = $state([]);

  function startEdit() {
    editValue = msg.content;
    editAttachments = [...(msg.attachments ?? [])];
    editing = true;
    queueMicrotask(() => editArea?.focus());
  }

  function removeEditAttachment(id) {
    editAttachments = editAttachments.filter((a) => a.id !== id);
  }

  let editDirty = $derived(
    editValue.trim() !== msg.content ||
      editAttachments.length !== (msg.attachments?.length ?? 0),
  );
  let editValid = $derived(editValue.trim().length > 0 || editAttachments.length > 0);

  async function commitEdit() {
    const content = editValue.trim();
    if (!editValid) return;
    const ids = editAttachments.map((a) => a.id);
    editing = false;
    if (!editDirty) return;
    await app.editMessage(msg.id, content, ids);
  }

  function onEditKeydown(e) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      commitEdit();
    } else if (e.key === 'Escape') {
      editing = false;
    }
  }

  // ---- vision-model image descriptions ----
  // When the chat model can't see images, the server substitutes a vision
  // model's text description. The user still sees the image itself, but can
  // peek at what the model actually received via a badge on the attachment
  // (lazy-fetched on first open, then cached).
  let descCache = $state({}); // attachment id -> description text ('' on error)
  let descBusy = $state({}); // attachment id -> in-flight
  async function loadDescription(att) {
    if (descCache[att.id] != null || descBusy[att.id]) return;
    descBusy[att.id] = true;
    try {
      descCache[att.id] = await api.attachmentDescription(att.id);
    } catch {
      descCache[att.id] = '';
    } finally {
      delete descBusy[att.id];
      descBusy = { ...descBusy };
    }
  }

  let waiting = $derived(
    isLive && !content && !reasoning && (toolCalls?.length ?? 0) === 0
  );

  // Any terminal problem that ended or cut off the response renders through
  // the single GenerationError component (same style for every cause).
  let generationError = $derived(status === 'failed' || status === 'stopped');
</script>

<!-- Per-image overlay for the gallery: the "what the model saw" badge shown
     when a vision model described this upload for a text-only chat model.
     Rendered as a SIBLING of each thumbnail button (ImageGallery never puts
     overlays inside it), so opening the popover doesn't also open the viewer.
     The badge positions itself; the gallery only supplies the relative cell. -->
{#snippet descBadge(att)}
  {#if att.has_description}
    <Popover.Root onOpenChange={(o) => o && loadDescription(att)}>
      <Popover.Trigger
        class="absolute bottom-1.5 right-1.5 flex size-6 items-center justify-center rounded-full bg-background/90 text-muted-foreground shadow-sm ring-1 ring-border/40 backdrop-blur transition-colors hover:text-foreground"
        title="What the model saw"
        aria-label="View the text description sent to the model"
      >
        <Eye class="size-3.5" strokeWidth={1.75} aria-hidden="true" />
      </Popover.Trigger>
      <Popover.Content align="end" class="w-72 p-3">
        <h4 class="mb-1.5 text-sm font-semibold">Image description</h4>
        <p class="mb-2 text-xs text-muted-foreground">This model can't see images — here's the description it received instead.</p>
        {#if descBusy[att.id]}
          <div class="flex items-center gap-2 text-xs text-muted-foreground"><Spinner class="size-3" label="Loading" /> Loading description…</div>
        {:else if descCache[att.id]}
          <pre class="max-h-48 overflow-y-auto whitespace-pre-wrap rounded-md bg-muted/50 p-2 text-xs">{descCache[att.id]}</pre>
        {:else}
          <p class="text-xs text-muted-foreground">No description available.</p>
        {/if}
      </Popover.Content>
    </Popover.Root>
  {/if}
{/snippet}

{#if msg.role === 'user'}
  <div class="group mt-5 flex justify-end">
    <div class="max-w-[85%] sm:max-w-[75%]">
      <div class="rounded-2xl rounded-br-sm bg-accent px-4 py-2.5">
        {#if (editing ? editAttachments : (msg.attachments ?? [])).length}
          {#if editing}
            <div class="mb-2 flex flex-wrap gap-1.5">
              {#each editAttachments as att (att.id)}
                <span class="relative inline-flex items-center gap-1 rounded-full border px-2.5 py-1 text-xs text-muted-foreground">
                  {#if att.kind === 'image'}
                    <img src={api.attachmentUrl(att.id)} alt={att.filename} class="size-5 rounded object-cover" />
                  {:else}
                    <Paperclip class="size-3" strokeWidth={1.75} aria-hidden="true" />
                  {/if}
                  <span class="max-w-32 truncate">{att.filename}</span>
                  <button
                    class="flex size-4 items-center justify-center rounded-full transition-colors hover:bg-foreground/10 hover:text-foreground"
                    title="Remove attachment"
                    onclick={() => removeEditAttachment(att.id)}
                  >
                    <X class="size-3" strokeWidth={1.75} aria-hidden="true" />
                  </button>
                </span>
              {/each}
            </div>
          {:else}
            <div class="mb-2 flex flex-col gap-1.5">
              <!-- Uploaded pictures get the same gallery as assistant replies;
                   the description badge rides along as a per-cell overlay. -->
              <ImageGallery
                items={userImageFiles}
                singleClass="max-h-40"
                widthClass="w-64 sm:w-80"
                overlay={descBadge}
              />
              {#if userFiles.length}
                <div class="flex flex-wrap gap-1.5">
                  {#each userFiles as att (att.id)}
                    <!-- Opens the lightbox rather than a new tab: the URL still
                         exists (and the viewer's download button exposes it),
                         but the click stays inside the app. -->
                    <button
                      type="button"
                      class="inline-flex items-center gap-1 rounded-full border px-2.5 py-1 text-xs text-muted-foreground transition-colors hover:text-foreground"
                      title={`View ${att.filename}`}
                      onclick={() => viewer.open(att)}
                    >
                      <Paperclip class="size-3" strokeWidth={1.75} aria-hidden="true" />
                      {att.filename}
                    </button>
                  {/each}
                </div>
              {/if}
            </div>
          {/if}
        {/if}
        {#if editing}
          <textarea
            bind:this={editArea}
            bind:value={editValue}
            class="min-h-16 w-full resize-none rounded-md border bg-background p-2 text-sm outline-none transition-colors focus:border-ring/50"
            onkeydown={onEditKeydown}
          ></textarea>
          <div class="mt-1.5 flex justify-end gap-1.5">
            <Button variant="ghost" size="sm" class="h-7 text-xs" onclick={() => (editing = false)}>Cancel</Button>
            <Button size="sm" class="h-7 text-xs" onclick={commitEdit} disabled={!editValid || !editDirty}>Save & resend</Button>
          </div>
        {:else if msg.content}
          <div class="whitespace-pre-wrap break-words text-[0.9375rem] leading-relaxed">{msg.content}</div>
        {/if}
      </div>
      <!-- No actions on a message that hasn't reached the server yet (app.outgoing). -->
      {#if !editing && msg.status !== 'outgoing'}
        <div class="mt-0.5 flex justify-end gap-0.5 opacity-0 transition-opacity group-hover:opacity-100">
          <CopyButton text={() => msg.content} label="Copy message" size="sm" />
          <IconButton icon={Pencil} label="Edit & resend" size="sm" onclick={startEdit} />
        </div>
      {/if}
    </div>
  </div>
{:else}
  <div class="group mt-6">
    {#if reasoning || waiting}
      <ThinkingBlock
        text={reasoning}
        streaming={isLive && status === 'generating' && !content}
        error={generationError && !content}
      />
    {/if}

    {#each toolCalls ?? [] as call (call.call_id)}
      <ToolCallItem {call} {status} />
    {/each}

    {#if content}
      <div bind:this={contentEl} class="md-body break-words">{@html html}{@html tailHtml}</div>
    {/if}

    <!-- Continuous from the moment of sending: MessageList shows the same
         cursor under app.outgoing, this one takes over once the reply is
         live, and it stays until `done` (also under the empty Thinking pill,
         so it doesn't blink out while the first token is on its way). -->
    {#if status === 'generating' && isLive}
      <Scramble />
    {/if}

    {#if imageFiles.length}
      <!-- Pictures the model gathered (show_image) as a gallery: tapping any
           cell opens the same lightbox as a single image, now with prev/next
           over the whole set. -->
      <ImageGallery items={imageFiles} class="mt-2" />
    {/if}

    {#if files.length}
      <div class="mt-2 flex flex-wrap gap-1.5">
        {#each files as att (att.id)}
          <!-- Opens the text viewer overlay; the download lives inside it
               (the viewer keeps the real URL on its download anchor). -->
          <button
            type="button"
            class="inline-flex items-center gap-1.5 rounded-lg border bg-muted/50 px-3 py-1.5 text-xs font-medium text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
            title={`View ${att.filename}`}
            onclick={() => viewer.open(att)}
          >
            <FileText class="size-3.5" strokeWidth={1.75} aria-hidden="true" />
            <span class="max-w-48 truncate">{att.filename}</span>
          </button>
        {/each}
      </div>
    {/if}

    {#if generationError}
      <GenerationError {status} error={errorText} />
    {/if}

    {#if !app.generating && !generationError && status !== 'generating'}
      <div class="mt-1.5 flex items-center gap-1 opacity-0 transition-opacity group-hover:opacity-100">
        {#if isLastAssistant}
          <IconButton icon={RotateCcw} label="Regenerate" onclick={() => app.regenerate()} />
        {/if}

        <CopyButton text={() => content} label="Copy message" />

        <Popover.Root>
          <Popover.Trigger
            class="flex size-7 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
            title="Generation info"
            aria-label="Generation info"
          >
            <Info class="size-3.5" strokeWidth={1.75} aria-hidden="true" />
          </Popover.Trigger>
          <Popover.Content align="start" class="w-64 p-3">
            <h4 class="mb-2 text-sm font-semibold">This response</h4>
            <dl class="space-y-1 text-xs">
              <div class="flex justify-between gap-2">
                <dt class="shrink-0 text-muted-foreground">Model</dt>
                <dd class="truncate font-mono font-medium" title={msgModel}>{msgModel || '—'}</dd>
              </div>
              <div class="flex justify-between gap-2">
                <dt class="text-muted-foreground">Duration</dt>
                <dd class="font-medium tabular-nums">{formatDuration(msg.duration_ms)}</dd>
              </div>
              <div class="flex justify-between gap-2">
                <dt class="text-muted-foreground">Input tokens</dt>
                <dd class="font-medium tabular-nums">{formatTokensOrDash(msg.prompt_tokens)}</dd>
              </div>
              <div class="flex justify-between gap-2">
                <dt class="text-muted-foreground">Output tokens</dt>
                <dd class="font-medium tabular-nums">{formatTokensOrDash(msg.completion_tokens)}</dd>
              </div>
            </dl>
          </Popover.Content>
        </Popover.Root>
      </div>
    {/if}
  </div>
{/if}
