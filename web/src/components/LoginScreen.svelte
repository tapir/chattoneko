<script>
  import { app } from '../lib/state.svelte.js';
  import { api } from '../lib/api.js';
  import { normalizeServerUrl } from '../lib/server.js';
  import logoUrl from '$lib/logo.svg';
  import { X } from '@lucide/svelte';
  import Spinner from './Spinner.svelte';
  import { Button } from '$lib/components/ui/button';
  import { Input } from '$lib/components/ui/input';
  import { Label } from '$lib/components/ui/label';

  // The web build is served by the Go backend itself (same-origin), so only
  // the native (Capacitor) build collects a server address — on this same
  // screen, above the credentials. Web renders the exact same screen without
  // the address field.
  const showServer = app.nativeApp;

  // First run / deliberate change-server opens in the address phase; a saved
  // server that later demands credentials (or the plain web build) starts
  // straight in the credentials phase with the address locked.
  let phase = $state(showServer && app.needsServerSetup ? 'address' : 'credentials');
  const credsPhase = $derived(phase === 'credentials');

  let url = $state(app.serverUrl);
  let username = $state('');
  let password = $state('');
  let error = $state('');
  let busy = $state(false);

  async function submit(e) {
    e.preventDefault();
    if (busy) return;
    error = '';
    if (!credsPhase) {
      const normalized = normalizeServerUrl(url);
      if (!normalized) {
        error = 'Enter a valid address, e.g. http://192.168.1.10:8080';
        return;
      }
      busy = true;
      try {
        const meta = await api.probeServer(normalized); // GET /api/meta, no auth
        if (meta?.auth_enabled) {
          // Server confirmed but auth-gated: lock the address and reveal the
          // credential fields right below it on this same screen.
          url = normalized;
          app.confirmServer(normalized);
          phase = 'credentials';
        } else {
          await app.setServer(normalized); // persists + boots the app
        }
      } catch (err) {
        error = err.status === 0
          ? 'Cannot reach that server — check the address and that the server is running'
          : `Server responded with an error (${err.status})`;
      } finally {
        busy = false;
      }
      return;
    }
    busy = true;
    try {
      await app.login(username.trim(), password);
    } catch (err) {
      error = err.status === 401 ? 'Invalid username or password' : err.message;
    } finally {
      busy = false;
    }
  }
</script>

<div class="login-glow flex min-h-screen items-center justify-center p-6 p-safe-pad">
  <!-- Change-server (opened over an existing server) can be bailed out of
       while the address is still editable. Pins to the SCREEN top-right.
       Absolute positioning ignores the container's padding, so the safe-area
       inset must be applied here explicitly (max() keeps the 1.25rem offset
       where there is no inset). -->
  {#if showServer && app.serverUrl && !credsPhase}
    <button
      type="button"
      class="absolute right-[max(1.25rem,env(safe-area-inset-right))] top-[max(1.25rem,env(safe-area-inset-top))] rounded-md p-2 text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
      title="Close without changes"
      onclick={() => app.cancelChangeServer()}
    >
      <X class="size-5" strokeWidth={1.75} aria-hidden="true" />
    </button>
  {/if}

  <!-- Nudged above dead-center; feels better optically on tall screens. -->
  <div class="w-full max-w-sm -translate-y-[6vh]">
    <div class="flex flex-col items-center pb-6 text-center">
      <img src={logoUrl} alt="Chattoねこ logo" class="mb-4 size-24" />
      {#if !credsPhase}
        <h2 class="text-xl font-semibold">Connect to a サーバー</h2>
        <p class="text-sm text-muted-foreground">Enter the address of your Chattoねこ server</p>
      {:else}
        <h2 class="text-xl font-semibold">Sign in to Chattoねこ</h2>
        <p class="text-sm text-muted-foreground">Enter your credentials to continue</p>
      {/if}
    </div>

    <form onsubmit={submit} class="space-y-4">
      {#if error}
        <div role="alert" class="rounded-lg border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive">
          {error}
        </div>
      {/if}
      {#if showServer}
        <div class="space-y-1.5">
          <Label for="server-url">Server address</Label>
          <Input
            id="server-url"
            type="text"
            class="h-9"
            bind:value={url}
            disabled={credsPhase}
            placeholder="http://192.168.1.10:8080"
            autocomplete="url"
            autocapitalize="none"
            autocorrect="off"
            spellcheck="false"
            required
          />
        </div>
      {/if}
      {#if credsPhase}
        <div class="space-y-1.5">
          <Label for="login-username">Username</Label>
          <Input id="login-username" type="text" class="h-9" bind:value={username} autocomplete="username" required />
        </div>
        <div class="space-y-1.5">
          <Label for="login-password">Password</Label>
          <Input id="login-password" type="password" class="h-9" bind:value={password} autocomplete="current-password" required />
        </div>
      {/if}
      <Button type="submit" class="h-9 w-full" disabled={busy}>
        {#if busy}
          <Spinner class="size-4" light label={credsPhase ? 'Signing in' : 'Connecting'} />
        {/if}
        {credsPhase ? 'Sign in' : 'Connect'}
      </Button>
    </form>
  </div>
</div>
