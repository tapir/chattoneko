<script>
  import { onMount } from 'svelte';
  import { initTheme } from './lib/theme.svelte.js';
  import { app } from './lib/state.svelte.js';
  import { isNative } from './lib/server.js';
  import { registerOverlay, closeTopOverlay } from './lib/overlays.svelte.js';
  import { Button } from '$lib/components/ui/button';
  import * as Sheet from '$lib/components/ui/sheet';
  import * as Tooltip from '$lib/components/ui/tooltip';
  import { Toaster } from '$lib/components/ui/sonner';
  import Sidebar from './components/Sidebar.svelte';
  import ChatView from './components/ChatView.svelte';
  import LoginScreen from './components/LoginScreen.svelte';
  import SettingsSheet from './components/SettingsSheet.svelte';
  import AttachmentViewer from './components/AttachmentViewer.svelte';
  import { viewer } from './lib/viewer.svelte.js';
  import Spinner from './components/Spinner.svelte';
  import ResizeHandle from './components/ResizeHandle.svelte';
  import { cubicOut } from 'svelte/easing';

  let route = $state(parseHash());
  let sidebarOpen = $state(false);
  // Desktop sidebar collapse (persisted). The header toggle collapses the
  // inline sidebar on lg+ screens and opens the mobile Sheet below lg.
  const SIDEBAR_KEY = 'chattoneko-sidebar-collapsed';
  let desktopSidebarCollapsed = $state(
    typeof localStorage !== 'undefined' && localStorage.getItem(SIDEBAR_KEY) === '1',
  );

  function toggleDesktopSidebar() {
    desktopSidebarCollapsed = !desktopSidebarCollapsed;
    try {
      localStorage.setItem(SIDEBAR_KEY, desktopSidebarCollapsed ? '1' : '0');
    } catch {
      /* private mode */
    }
  }

  // User-resizable desktop sidebar width (persisted by ResizeHandle).
  let sidebarWidth = $state(288);

  // The mobile sidebar sheet is an overlay like any other: register it so
  // the Android back button closes it (topmost-first).
  $effect(() => {
    if (sidebarOpen) return registerOverlay(() => (sidebarOpen = false));
  });

  // Desktop inline-sidebar slide-in/out. Svelte's built-in `slide` can't be
  // used: the wrapper enforces min-w-max (the drag floor), which would clamp
  // the width and kill the animation. This variant overrides min-width for
  // the duration of the transition and animates width only (no height
  // collapse on a full-height panel).
  function widthSlide(node, { duration = 200 } = {}) {
    const w = node.getBoundingClientRect().width;
    return {
      duration,
      easing: cubicOut,
      // min-width/overflow travel inside the keyframes so they only apply
      // while animating; afterwards the class min-w-max (the drag floor)
      // governs again.
      css: (t) =>
        `width: ${(w * t).toFixed(1)}px; opacity: ${t}; min-width: 0; overflow: hidden`,
    };
  }

  function parseHash() {
    const h = location.hash || '#/';
    const m = h.match(/^#\/c\/([^/]+)/);
    return m ? { name: 'chat', id: m[1] } : { name: 'home' };
  }

  // True after the first route application (fresh page load / first login).
  let initialRouteApplied = false;

  // Per-tab "did this tab already load the app" flag. sessionStorage is
  // scoped to a top-level tab and survives reloads and browser tab/session
  // restore, but is empty in a brand-new tab. That lets us tell an
  // explicit deep link (pasted/shared into a fresh tab) apart from a
  // browser-restored hash: only the latter should land on the new-chat
  // screen, while a real #/c/<id> link must open its chat.
  let restoredTab = false;
  const BOOT_KEY = 'chattoneko-booted';
  try {
    restoredTab = sessionStorage.getItem(BOOT_KEY) === '1';
    sessionStorage.setItem(BOOT_KEY, '1');
  } catch {
    /* storage unavailable (private mode) -> treat as a fresh deep link */
  }

  function applyRoute(opts = {}) {
    // Parse into a LOCAL first: reading `route` (a $state) inside the
    // auth-gated $effect below while also writing it caused an infinite
    // effect_update_depth_exceeded loop that broke the whole app.
    let r = parseHash();
    // Fresh page load or first login: if this tab already ran the app
    // (reload / browser session restore), the restored hash points at the
    // last visited chat (#/c/…) and we land on the new-chat screen —
    // never auto-restore the most recent chat. A brand-new tab means the
    // hash came from an explicit deep link, so honour it.
    if (opts.initial && r.name === 'chat' && restoredTab) {
      history.replaceState(null, '', '#/');
      r = { name: 'home' };
    }
    route = r;
    // Close the mobile sidebar whenever the route changes.
    sidebarOpen = false;
    if (r.name === 'chat') {
      app.openChat(r.id);
    } else {
      app.closeChat();
    }
  }

  onMount(() => {
    initTheme(); // re-sync theme (head script already seeded it pre-paint)
    app.init(); // route is applied by the auth-gated $effect below
    // Dynamic import so the web bundle never loads Capacitor plugins
    // (same pattern as the camera/file-picker/status-bar plugins). The
    // resolved App plugin is kept for exitApp() in onAndroidBack.
    if (isNative())
      import('@capacitor/app').then(({ App }) => {
        capacitorApp = App;
        App.addListener('backButton', onAndroidBack);
      });
  });

  // The Capacitor App plugin, resolved by the dynamic import above (native
  // only; null on the web build where the back button never fires).
  let capacitorApp = null;

  // Android back button/gesture (native only). Registering the listener
  // overrides Capacitor's default handling, so the fallback (hash history
  // back, then exit) is implemented here too. Overlays close first:
  // server-switch screen, then the mobile sidebar sheet.
  function onAndroidBack() {
    if (app.needsServerSetup && app.serverUrl) {
      app.cancelChangeServer();
      sidebarOpen = false; // don't re-open the sheet the gear was tapped from
      return;
    }
    if (closeTopOverlay()) return;
    if (location.hash && location.hash !== '#/') history.back();
    else capacitorApp?.exitApp();
  }

  // ChatHeader's menu button (data-sidebar="trigger") has no direct access
  // to this component's state, so a window-level click listener routes it:
  // desktop (lg+) collapses the inline sidebar; mobile opens the Sheet.
  function openSidebarFromTrigger(e) {
    const trigger = e.target?.closest?.('[data-sidebar="trigger"]');
    if (!trigger) return;
    if (window.matchMedia('(min-width: 1024px)').matches) {
      toggleDesktopSidebar();
    } else {
      sidebarOpen = true;
    }
  }

  // Opening a chat after signing in (init runs before auth).
  $effect(() => {
    if (app.authChecked && app.authed) {
      applyRoute({ initial: !initialRouteApplied });
      initialRouteApplied = true;
    }
  });
</script>

<svelte:window onhashchange={() => applyRoute()} onfocus={() => app.onFocus()} onclick={openSidebarFromTrigger} />

{#if !app.authChecked}
  <div class="flex min-h-screen items-center justify-center p-safe-pad">
    <Spinner class="size-8" />
  </div>
{:else if app.needsServerSetup || (app.authEnabled && !app.authed)}
  <!-- One screen for both gates: native adds the server-address field on
       top; web renders the same card without it. -->
  <LoginScreen />
{:else if app.serverDown}
  <div class="flex min-h-screen items-center justify-center p-safe-pad">
    <div class="flex w-full max-w-md flex-col items-center gap-4 rounded-xl border bg-card p-8 text-center shadow-sm">
      <div class="space-y-1.5">
        <div class="text-base font-semibold">Cannot reach the server</div>
        <p class="text-sm text-muted-foreground">
          The backend did not respond to <code class="font-mono text-xs">/api/meta</code>. Start the server and
          reload.
        </p>
      </div>
      <div class="flex gap-2">
        <Button variant="outline" size="sm" onclick={() => location.reload()}>Reload</Button>
        {#if app.nativeApp}
          <Button variant="outline" size="sm" onclick={() => app.changeServer()}>Change server</Button>
        {/if}
      </div>
    </div>
  </div>
{:else}
  <Tooltip.Provider delayDuration={400}>
    <div class="flex h-dvh overflow-hidden bg-background text-foreground p-safe">
    <!-- Desktop sidebar (user-resizable via the right-edge drag handle) -->
    {#if !desktopSidebarCollapsed}
      <!-- min-w-max: the sidebar can never be dragged narrower than its
           content (header row: logo + title + gap + buttons); the chat list
           is excluded via contain:inline-size on its <nav>. ResizeHandle's
           numeric min stays as the drag-state floor. The widthSlide
           transition overrides min-width via keyframes while it runs. -->
      <div class="relative hidden h-full min-w-max shrink-0 lg:block" style="width: {sidebarWidth}px" transition:widthSlide>
        <Sidebar />
        <ResizeHandle
          bind:width={sidebarWidth}
          storageKey="chattoneko-sidebar-width"
          fallback={288}
          min={200}
          max={480}
          label="Resize sidebar"
        />
      </div>
    {/if}

    <!-- Mobile sidebar -->
    <Sheet.Root bind:open={sidebarOpen}>
      <!-- Fullscreen on mobile: the !important overrides beat the Sheet
           base classes (w-3/4, sm:max-w-sm). border-r-0! kills the right
           border (base data-[side=left]:border-r) that otherwise rendered
           as a stray 1px line at the right screen edge when fullscreen.
           p-safe keeps the header row clear of the status bar when the
           WebView is laid out edge-to-edge (native). The sheet's absolute
           X is disabled — it overlapped the New button; Sidebar renders
           its own close button in the header row, centered with New.
           Safe-area padding comes from the sheet-content primitive. -->
      <Sheet.Content side="left" class="w-full! max-w-full! gap-0 border-r-0! bg-sidebar p-0" showCloseButton={false}>
        <Sheet.Title class="sr-only">Chats</Sheet.Title>
        <Sidebar onClose={() => (sidebarOpen = false)} />
      </Sheet.Content>
    </Sheet.Root>

    <div class="flex h-full min-w-0 flex-1 flex-col">
      {#key route.name === 'chat' ? route.id : 'home'}
        <ChatView />
      {/key}
    </div>
    </div>

    <!-- Server settings overlay. Auto-opens (and locks) when setup is
         incomplete; otherwise reachable from the sidebar / header. -->
    <SettingsSheet />
  </Tooltip.Provider>
{/if}

<!-- Attachment lightbox (text / image), opened by clicking any attachment
     in a message. <dialog> lives in the browser top layer, so mounting it
     at the root is about lifetime only, not stacking. `items` is the
     message's image set: with more than one, the viewer becomes a gallery
     (swipe / arrows / chevrons) that just re-points `viewer.attachment`.
     Deliberately NOT keyed on the attachment id — navigating must reuse the
     mounted dialog, not tear it down and rebuild it. -->
{#if viewer.attachment}
  <AttachmentViewer
    attachment={viewer.attachment}
    items={viewer.items}
    onclose={() => viewer.close()}
    onprev={() => viewer.prev()}
    onnext={() => viewer.next()}
  />
{/if}

<Toaster position="bottom-right" />
