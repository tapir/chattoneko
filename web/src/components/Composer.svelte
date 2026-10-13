<script>
  import { app } from '../lib/state.svelte.js';
  import { onMount } from 'svelte';
  import { Mic, Paperclip, SendHorizontal, Square, Trash2, X } from '@lucide/svelte';
  import Spinner from './Spinner.svelte';
  import { Button } from '$lib/components/ui/button';
  import * as Select from '$lib/components/ui/select';
  import ModelPickerSheet from './ModelPickerSheet.svelte';
  import AttachmentSheet from './AttachmentSheet.svelte';
  import { isNative } from '../lib/server.js';
  import { viewer } from '../lib/viewer.svelte.js';
  import { capturePhoto, pickPhotos, pickFiles } from '../lib/native-attachments.js';
  import { micAvailable, startRecording } from '../lib/record.js';

  // Draft text lives in the store (keyed by chat id) so it survives the
  // Composer remount that ensureChat() triggers on the first send, and chat
  // switches. Same for pending attachments, which are staged client-side
  // (File objects) and only uploaded when the message is actually sent.
  let pending = $derived(app.pendingList());
  // Staged pictures are the lightbox's gallery set, so opening one chip lets
  // you swipe through the others before sending (as with a sent message).
  // Text files stay out: they have no server copy, and the viewer reads those
  // by id.
  let stagedImages = $derived(
  pending.filter((a) => a.kind === 'image' && a.previewUrl),
);
  let sending = $state(false);
  let fileInput = $state(null);
  let textArea = $state(null);
  // Native (Capacitor): the attach button opens a bottom drawer with
  // camera/photos/files shortcuts instead of the plain file input.
  let attachOpen = $state(false);
  let attachBusy = $state(false);

  // New (not-yet-created) chat: focus the prompt on mount so the mobile
  // soft keyboard pops up and desktop users can type right away. The first
  // send of a new chat remounts this component (ensureChat changes the route
  // key), which throws away the focus send() just set and drops the keyboard
  // — app.outgoing identifies that remount, so take the focus back.
  onMount(() => {
    if (app.voiceOutgoing) {
      // A voice send's remount: leave the keyboard as the user had it.
      if (app.voiceFocus) textArea?.focus();
      return;
    }
    if (app.activeChatId == null || app.outgoing) textArea?.focus();
  });

  // Model selection: for an existing chat this patches the chat's model; for a
  // not-yet-created chat it sets the draft model used at creation.
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
  // model's configured default. Invalid selections (e.g. after a model
  // switch) fall back to the default rather than showing a stale value.
  let currentEffort = $derived.by(() => {
    const chosen = app.chat ? app.chat.params?.reasoning_effort : app.newChatEffort;
    const opts = app.effortOptionsFor(currentModel);
    if (chosen && opts.includes(chosen)) return chosen;
    return app.defaultEffortFor(currentModel);
  });

  // Switching model snaps to that model's configured default effort.
  function handleModelChange(value) {
    if (!value || value === currentModel) return;
    const def = app.defaultEffortFor(value);
    if (app.chat) {
      const params = { ...(app.chat.params ?? {}) };
      if (def) params.reasoning_effort = def;
      else delete params.reasoning_effort;
      app.patchChat({ model: value, params });
    } else {
      app.newChatModel = value;
      app.newChatEffort = def;
      app.newChatModelTouched = true; // explicit draft choice: stop following the default
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

  // Sending waits on the chat still loading: ensureChat() would see no
  // chat object and create a NEW chat, delivering the message to the wrong
  // conversation and abandoning the one being opened.
  let canSend = $derived(
    (app.draftFor().trim().length > 0 || pending.length > 0) &&
      !sending &&
      !app.chatLoading,
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
      // user doesn't lose them. ensureChat() may have remounted this Composer —
      // the store-backed draft lands in the NEW instance.
      app.setDraft(content);
      for (const a of staged) app.restorePendingAttachment(a);
      requestAnimationFrame(autoGrow);
    } finally {
      sending = false;
    }
  }

  // Clipboard paste: images (and files) land as clipboardData.files, not as
  // insertable text — stage them as pending attachments instead of letting the
  // paste drop binary gibberish into the draft. Staging is client-side only
  // (no network — the text sniff reads local bytes), so both handlers fire it
  // and forget; addAttachments reports its own rejects by toast.
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

  // ---- push to talk (lib/record.js) ----

  // A hold that never ends would leave the mic live until the cap, so the cap
  // reports success rather than dropping what was said.
  const MAX_SECONDS = 300;
  // Leftward drag off the button arms the cancel at the threshold and throws the
  // take away at the end of the drag — the strip follows the finger only this
  // far, so it is dragged clear of the composer without flying off a wide row.
  const SLIDE_CANCEL_PX = 60;
  const DRAG_MAX_PX = 160;
  const MIC_HINT = 'Hold to record';
  const MIC_OFF_HINT = 'Microphone needs HTTPS';
  const REDUCED = matchMedia('(prefers-reduced-motion: reduce)').matches;

  // Same breakpoint the composer layout switches at (sm), so the mic only takes
  // the send button's slot where the row has no width to spare for both.
  const NARROW = '(max-width: 639px)';
  let narrow = $state(matchMedia(NARROW).matches);
  // getUserMedia/MediaRecorder are secure-context only, so a plain-HTTP LAN page
  // has no mic to offer (lib/record.js). Undefined immediately, never refetched.
  const micOk = micAvailable();
  let cardEl = $state(null);
  let recording = $state(false);
  let elapsed = $state(0);
  let drag = $state(0); // how far the strip has been pulled left, in px
  let stripEl = $state(null);
  let rec = null; // the live recorder handle; not reactive
  let timer = null;
  let startedAt = 0;
  let pressX = 0;
  let pressId = null; // the pointer the hold belongs to
  let discarding = false; // the slide-out is playing; the mic is already off

  $effect(() => {
    const mq = matchMedia(NARROW);
    const onChange = () => (narrow = mq.matches);
    mq.addEventListener('change', onChange);
    return () => mq.removeEventListener('change', onChange);
  });

  // A recording must not outlive the composer: the send it ends in would land
  // in whatever chat is active when it finishes.
  $effect(() => () => finishRecording(true));

  let emptyInput = $derived(app.draftFor().trim().length === 0 && pending.length === 0);
  // Below sm an empty composer is all mic and no send: there is nothing to send,
  // and the button is where the right thumb already is. At sm and up the row has
  // room for both.
  let showMic = $derived(
    !sending && (recording || !narrow || (emptyInput && !app.generating)),
  );
  let showSend = $derived(!narrow || !showMic);
  let clock = $derived(
    `${Math.floor(elapsed / 60)}:${String(Math.floor(elapsed % 60)).padStart(2, '0')}`,
  );
  // Past the threshold the strip turns into the cancel it has been offering.
  let armed = $derived(drag >= SLIDE_CANCEL_PX);

  // Capture goes on the CARD rather than on the button: the card is the one part
  // of the composer a hold never re-renders, and a capture on a node that goes
  // away is released — which would end the recording on its first frame.
  function onMicDown(e) {
    if (e.button !== 0) return;
    // Keeps focus (and the soft keyboard) off the button, and stops the
    // browser's own press-and-hold selection.
    e.preventDefault();
    // Pressing again while it records ends the take: a live one sends, one
    // still waiting on the permission prompt cancels.
    if (recording) return endPress();
    // Capture keeps the press alive over the strip that covers the row.
    // Safari can refuse it for a touch pointer; the window listeners end the
    // press either way.
    try {
      cardEl?.setPointerCapture(e.pointerId);
    } catch {}
    pressId = e.pointerId;
    pressX = e.clientX;
    beginRecording();
  }

  function onMicKeyDown(e) {
    if ((e.key !== ' ' && e.key !== 'Enter') || e.repeat) return;
    e.preventDefault();
    beginRecording();
  }

  function onPressMove(e) {
    if (!recording) return;
    drag = Math.min(Math.max(pressX - e.clientX, 0), DRAG_MAX_PX);
    // The end of the drag IS the cancel — no release to wait for.
    if (drag >= DRAG_MAX_PX) discardSlide();
  }

  async function beginRecording() {
    if (recording || sending || !micOk) return;
    elapsed = 0;
    drag = 0;
    // Up before the await, because the mic permission prompt is a screen the
    // user has to answer while already holding the button.
    recording = true;
    let handle;
    try {
      handle = await startRecording();
    } catch (err) {
      recording = false;
      app.toast('error', `Couldn't record: ${err?.message ?? 'unknown error'}`);
      return;
    }
    // Cancelled (or unmounted) while the microphone was coming up: it is live
    // now, so it has to be released even though there is nothing to stage.
    if (!recording) return handle.cancel();
    rec = handle;
    startedAt = Date.now();
    timer = setInterval(() => {
      elapsed = (Date.now() - startedAt) / 1000;
      if (elapsed >= MAX_SECONDS) finishRecording(false);
    }, 200);
  }

  // Only a pointer event carries a pointerId, and only the hold's own pointer
  // ends it: a second finger's up is not this press' release.
  function endPress(e) {
    if (!recording || (e?.pointerId != null && e.pointerId !== pressId)) return;
    // The press ended before the microphone came live — the permission prompt
    // cutting in is the usual reason. Cancel rather than record on with no
    // finger holding the button; beginRecording releases the stream when it
    // arrives to nothing.
    if (!rec) {
      recording = false;
      return;
    }
    // Dragged back under the threshold means "keep it", as it does in every
    // messenger that has this gesture.
    if (armed) return discardSlide();
    finishRecording(false);
  }

  // Stops the recorder and the clock without touching the strip, so the caller
  // can still animate it out. Resolves to the recording's stop promise.
  function releaseMic(cancel) {
    clearInterval(timer);
    timer = null;
    const handle = rec;
    rec = null;
    if (!handle) return null;
    return cancel ? handle.cancel() : handle.stop();
  }

  // The release IS the send — no staged chip in between. The take is shaped
  // like a pending entry so app.send() and the bubble treat it exactly like a
  // picked file; actually staging it first would paint the chip for a frame,
  // because addAttachments has an async classify inside.
  async function finishRecording(cancel) {
    if (!recording) return;
    const stopped = releaseMic(cancel);
    recording = false;
    drag = 0;
    if (cancel || !stopped) return;
    const file = await stopped;
    if (!file) return app.toast('error', "Couldn't record");
    // Sending mid-chat-load would deliver the take to a NEW chat (the guard
    // send() keeps); let it wait as an ordinary chip instead.
    if (sending || app.chatLoading) return app.addAttachments([file]);
    const take = {
      id: crypto.randomUUID(),
      file,
      filename: file.name,
      size: file.size,
      kind: 'file',
      previewUrl: '',
    };
    sending = true;
    app.voiceOutgoing = true;
    app.voiceFocus = document.activeElement === textArea;
    try {
      await app.send('', [take]);
    } catch {
      // toast already shown by state; stage the take so it is not lost.
      app.addAttachments([file]);
    } finally {
      sending = false;
      app.voiceOutgoing = false;
    }
  }

  // A take being thrown away has to be SEEN to be thrown away: the mic is off
  // already and the strip slides out left and fades while the composer waits for
  // it. WAAPI rather than a CSS transition, because the transition property and
  // the transform it should animate would land in the same style change and the
  // browser would skip the animation.
  function discardSlide() {
    if (discarding) return;
    discarding = true;
    releaseMic(true);
    stripEl?.animate(
      [
        { transform: `translateX(${-drag}px)`, opacity: 1 },
        { transform: 'translateX(-110%)', opacity: 0 },
      ],
      { duration: 160, easing: 'ease-out', fill: 'forwards' },
    );
    shakeCard();
    setTimeout(() => {
      recording = false;
      drag = 0;
      discarding = false;
    }, 170);
  }

  // The take was thrown away, and the composer is what says so: the card shakes,
  // not the viewport — the card is what the gesture acted on, and a shaking
  // screen is exactly the motion prefers-reduced-motion exists for.
  function shakeCard() {
    if (REDUCED) return;
    // no-op where the API is missing (iOS) or there is no motor (desktop);
    // the app's WebView needs the declared VIBRATE permission to honour it
    navigator.vibrate?.(30);
    cardEl?.animate(
      [
        { transform: 'translateX(0)' },
        { transform: 'translateX(-11px)' },
        { transform: 'translateX(9px)' },
        { transform: 'translateX(-7px)' },
        { transform: 'translateX(5px)' },
        { transform: 'translateX(-3px)' },
        { transform: 'translateX(2px)' },
        { transform: 'translateX(0)' },
      ],
      { duration: 520, easing: 'ease-out' },
    );
  }

  // The press is captured by the card, and the button it started on is covered
  // by the strip a moment later, so the release is read from the window:
  // pointerup for the hold, keyup for one that started on the keyboard. Only the
  // pointer that started the hold counts, so a second finger's up is not this
  // press' release.
  //
  // Everything that is NOT a clean release is an interruption, and an
  // interrupted hold is a cancelled hold, never a sent one. The window swallows
  // contextmenu while recording because Chrome hit-tests the long-press menu at
  // the FINGER, not at the capture target, and a holding finger drifts off the
  // small button — and a menu that appears reclaims the touch as pointercancel,
  // half a second into the hold (the rule lib/longpress.js keeps too). What
  // still reclaims it — system navigation, browser chrome, dialogs, the app
  // switcher — arrives as pointercancel, as blur, or as the page going hidden,
  // and all three cancel.
  $effect(() => {
    if (!recording) return;
    const up = (e) => {
      if (e.pointerId === pressId) endPress();
    };
    const move = (e) => {
      if (e.pointerId === pressId) onPressMove(e);
    };
    const keyUp = (e) => {
      if (e.key === ' ' || e.key === 'Enter') endPress();
    };
    const keyDown = (e) => {
      if (e.key === 'Escape') finishRecording(true);
    };
    const interrupted = () => finishRecording(true);
    const noMenu = (e) => e.preventDefault();
    const hidden = () => {
      if (document.hidden) interrupted();
    };
    window.addEventListener('pointerup', up);
    window.addEventListener('pointermove', move);
    window.addEventListener('pointercancel', interrupted);
    window.addEventListener('keyup', keyUp);
    window.addEventListener('keydown', keyDown);
    window.addEventListener('blur', interrupted);
    window.addEventListener('contextmenu', noMenu);
    document.addEventListener('visibilitychange', hidden);
    return () => {
      window.removeEventListener('pointerup', up);
      window.removeEventListener('pointermove', move);
      window.removeEventListener('pointercancel', interrupted);
      window.removeEventListener('keyup', keyUp);
      window.removeEventListener('keydown', keyDown);
      window.removeEventListener('blur', interrupted);
      window.removeEventListener('contextmenu', noMenu);
      document.removeEventListener('visibilitychange', hidden);
    };
  });
</script>

<div class="shrink-0 px-3 pb-4 pt-2 sm:px-6">
  <div class="mx-auto max-w-4xl">
    <div
      bind:this={cardEl}
      class="rounded-2xl border bg-card shadow-sm transition-colors focus-within:border-ring/50"
    >
      {#if pending.length > 0}
        <div class="flex flex-wrap items-center gap-1.5 px-3 pt-3">
          {#each pending as att (att.id)}
            <span class="inline-flex h-7 items-center gap-1.5 rounded-full bg-accent px-2.5 text-xs">
              {#if att.kind === 'image' && att.previewUrl}
                <!-- Everything but ✕ opens the lightbox: a staged image only
                     has a local object URL, which is what AttachmentViewer
                     prefers, so this works before the file is uploaded.
                     Nested buttons are invalid, hence the sibling pair. -->
                <button
                  type="button"
                  class="inline-flex min-w-0 items-center gap-1.5 rounded-full"
                  title={`View ${att.filename}`}
                  aria-label={`View ${att.filename}`}
                  onclick={() => viewer.open(att, stagedImages)}
                >
                  <img src={att.previewUrl} alt={att.filename} class="size-5 rounded object-cover" />
                  <span class="max-w-40 truncate">{att.filename}</span>
                </button>
              {:else}
                <Paperclip class="size-3" strokeWidth={1.75} aria-hidden="true" />
                <span class="max-w-40 truncate">{att.filename}</span>
              {/if}
              <button
                class="flex size-4 shrink-0 items-center justify-center rounded-full transition-colors hover:bg-foreground/10"
                title="Remove attachment"
                aria-label={`Remove ${att.filename}`}
                onclick={() => removePending(att.id)}
              >
                <X class="size-3" strokeWidth={1.75} aria-hidden="true" />
              </button>
            </span>
          {/each}
        </div>
      {/if}

      <div class="flex items-end gap-1.5 p-2.5">
        <!-- The recording strip OVERLAYS the attach button + textarea instead
             of replacing them: unmounting the row would move everything the
             held finger is sitting on, and it would blur the textarea (the
             soft keyboard would drop mid-hold). Same row, same height, no
             shift — the pressed button never goes anywhere. -->
        <div class="relative flex min-w-0 flex-1 items-end gap-1.5">
          <Button
            variant="ghost"
            size="icon"
            class="size-9 shrink-0 rounded-full"
            title="Attach files (images, text/code, audio, pdf)"
            onclick={onAttachButton}
            disabled={attachBusy}
          >
            {#if attachBusy}
              <Spinner class="size-4" label="Attaching" />
            {:else}
              <Paperclip class="size-[18px]" strokeWidth={1.75} aria-hidden="true" />
            {/if}
          </Button>
          <!-- No accept="": browsers map it through the OS extension→MIME table,
               which has no entry for .go/.toml and calls .ts a video, so any
               allow-list hides files the server would accept. addAttachments()
               filters by extension and content instead. -->
          <input bind:this={fileInput} type="file" class="hidden" multiple onchange={onFiles} />

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

          {#if recording}
            <div
              class="absolute inset-0 flex touch-none select-none items-center overflow-hidden bg-card pl-1"
            >
              <!-- items-baseline, not items-center: equal line boxes do NOT put
                   a mono clock and a sans hint on one baseline (their fonts'
                   ascent ratios differ), and this has to hold on every machine's
                   fonts. The icon slot opts out with self-center, and the
                   overlay centers the whole baseline group. -->
              <div bind:this={stripEl} class="flex items-baseline gap-2.5">
                <!-- One fixed slot: the pulsing dot while it records, the trash
                     once the drag arms — so arming shifts nothing, and nothing
                     is reserved for a control that is not there. -->
                <span
                  class="flex size-4 shrink-0 items-center justify-center self-center text-destructive"
                >
                  {#if armed}
                    <Trash2 class="size-4" strokeWidth={1.75} aria-hidden="true" />
                  {:else}
                    <span class="size-2.5 rounded-full bg-destructive animate-pulse"></span>
                  {/if}
                </span>
                <span
                  class="shrink-0 font-mono text-[0.9375rem] leading-6 tabular-nums {armed
                    ? 'text-destructive'
                    : ''}"
                >
                  {clock}
                </span>
                <!-- The hint slides into this box and is clipped by its left
                     edge, so it disappears into the gap instead of running over
                     the clock. Same size as the prompt it replaces. -->
                <span class="relative min-w-0 flex-1 overflow-hidden">
                  <span
                    class="block truncate pl-3 text-[0.9375rem] leading-6 text-muted-foreground"
                    style="transform: translateX({-drag}px)"
                  >
                    ‹ Slide to cancel
                  </span>
                </span>
              </div>
            </div>
          {/if}
        </div>

        {#if showMic}
          {#if micOk}
            <Button
              variant={recording ? 'destructive' : narrow ? 'default' : 'ghost'}
              size="icon"
              class="size-9 shrink-0 touch-none rounded-full [-webkit-touch-callout:none]"
              title={MIC_HINT}
              aria-label={MIC_HINT}
              onpointerdown={onMicDown}
              onkeydown={onMicKeyDown}
            >
              <Mic class="size-[18px]" strokeWidth={1.75} aria-hidden="true" />
            </Button>
          {:else}
            <!-- A span rather than a disabled button on purpose: the kit's
                 disabled:pointer-events-none would swallow the hover that tells
                 the user why there is no mic here. -->
            <span
              class="inline-flex size-9 shrink-0 items-center justify-center rounded-full opacity-50"
              role="button"
              aria-disabled="true"
              title={MIC_OFF_HINT}
              aria-label={MIC_OFF_HINT}
            >
              <Mic class="size-[18px]" strokeWidth={1.75} aria-hidden="true" />
            </span>
          {/if}
        {/if}

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
        {:else if showSend}
          <Button
            size="icon"
            class="size-9 shrink-0 rounded-full"
            title="Send"
            onclick={send}
            disabled={!canSend || recording}
          >
            {#if sending}
              <Spinner class="size-4" label="Sending" />
            {:else}
              <SendHorizontal class="size-4" strokeWidth={1.75} aria-hidden="true" />
            {/if}
          </Button>
        {/if}
      </div>

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
