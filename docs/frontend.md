# Frontend Architecture

The web UI is a Svelte 5 single-page app, built with Vite into `web/dist` and embedded into the Go binary at compile time. In the browser it is a plain same-origin client of the backend's REST + SSE API; the only build-time coupling to the server is that embedding. The same codebase also runs inside the Android app (see `docs/mobile.md`) — the SPA detects the native WebView at runtime and switches into "bring your own server" mode.

## Stack

- **Svelte 5** with runes (`$state`, `$derived`, `$effect`, `$props`, `$bindable`, `SvelteSet`) — plain JavaScript, no TypeScript, no preprocessor.
- **Vite 8** (rolldown) — dev server proxies `/api` to `localhost:8080`; the production build splits the heavy markdown pipeline (`incremark-renderer` plus its `marked` + `katex` + `highlight.js` + `xss` deps, ~153 kB gzipped) into its own chunk via the function form of `manualChunks`, and `chunkSizeWarningLimit` is raised to 600 kB because a single-view chat app has no route to split the entry along. A resolve alias pins the bare `highlight.js` specifier to `highlight.js/lib/common`: incremark imports the full ~190-language build, and the app only ever highlighted the common set — worth ~250 kB gzipped in the lazy chunk. The alias must be the exact-match regex form; a string alias also rewrites `highlight.js/lib/common` into a doubled path.
- **Tailwind CSS 4** (+ `tw-animate-css`), **bits-ui** primitives behind local shadcn-style components (`src/lib/components/ui/*` — badge, button, drawer, input, label, popover, select, sheet, skeleton, sonner toasts, switch, toggle, toggle-group, tooltip; see **UI kit** below). **vaul-svelte** backs the drawer, which the mobile bottom sheets are built on.
- **Markdown pipeline**: `incremark-renderer` — marked-based incremental lexer → KaTeX in MathML mode (LaTeX) → highlight.js (code) → xss (sanitization), lazily loaded (see **Markdown**).
- Icons: `@lucide/svelte`, imported per component.
- Capacitor plugins (`@capacitor/app`, `@capacitor/camera`, `@capacitor/status-bar`, `@capawesome/capacitor-file-picker`) are dependencies of `web/` too, and every one of them is reached only through a dynamic `import()` guarded by `isNative()`, so the web bundle never evaluates them.

## UI kit (shadcn-style components)

`src/lib/components/ui/*` are shadcn-svelte-style components over bits-ui primitives, **owned by this repo** — nothing keeps them in sync with any registry. Their conventions: `data-slot` attributes, the `data-icon="inline-start/end"` spacing convention, `xs/sm/lg` sizes, `rounded-lg`, ToggleGroup spacing via `--gap`.

> **Do NOT run `npx shadcn-svelte update` or re-`add` existing components.** The registry configured in `components.json` (`https://shadcn-svelte.com/registry`, style `new-york`) serves markup that contradicts the conventions above (e.g. button: `h-9` / `rounded-md` / `has-[>svg]` padding; toggle: no `data-icon` hooks, no spacing system). Fetching from it overwrites the local files and visually breaks the whole app. Treat the directory as plain source.

Editing rules for these components:

- **Semantic tokens only** (`bg-accent`, `text-muted-foreground` — never raw colors or manual `dark:` overrides), and follow the conventions in `.pi/skills/shadcn-svelte` (composition, `class` for layout only, `cn()` for conditionals). Selected/active states use `--accent` (+ `--accent-foreground`), which is what `toggle.svelte` keys `data-[state=on]` off.
- **Portaled overlay content sits at `z-[105]`** (select/popover/tooltip content), above the app's own modals (`SettingsSheet` at `z-[100]`) so a portaled dropdown opened from inside one is visible. Sheet/drawer chrome sits at `z-50`. Native `<dialog>` modals (`ConfirmModal`, `AttachmentViewer`) use the browser top layer and need no z-index at all.
- **The toggle-group root emits `data-horizontal`/`data-vertical`** (derived from `orientation`) in addition to bits-ui's `data-orientation`: the item variants (joined borders, first/last rounding) and the root's `data-vertical:` classes key off those two attributes.

## Source layout (web/src)

```
main.js                     mounts App
App.svelte                  shell: routing, theme, auth gates, layout, Android back button
app.css                     theme tokens, markdown styles, safe-area + press feedback, animations
lib/
  state.svelte.js           the app store — ALL application state + logic (~1100 lines)
  api.js                    REST client + response normalizers
  stream.svelte.js          SSE managers (ChatStream, GlobalStream, TitleStream)
  server.js                 runtime server config (native build) + JWT storage/expiry + stream URLs
  markdown.js               façade: lazy loader + pure string helpers (escapeHtml, normalizeHeadings, splitHeadingHold)
  markdown.impl.js          the heavy pipeline (incremark-renderer options + DOM renderer factory) — own chunk
  native-attachments.js     native camera / gallery / file pickers → File[]
  overlays.svelte.js        LIFO registry of open overlays (Android back button)
  typewriter.js             typewriter animation for streamed text
  theme.svelte.js           dark/light theme (localStorage + live system preference + status bar)
  viewer.svelte.js          singleton state for the attachment lightbox (open/close/step)
  format.js, clipboard.js, resize.js, utils.js (cn)
  logo.svg                  sidebar + login-screen mark (also the favicon via index.html)
  neko*.png                 cat art, one picked at random for the empty-chat screen
components/
  ChatView.svelte           header + message list + composer column
  ChatHeader.svelte         title, token/context stats, 3-dot menu, Logs / Tools / System panels
  MessageList.svelte        display-item construction, auto-scroll
  MessageItem.svelte        one message: markdown, thinking, tool calls, files, actions
  Composer.svelte           prompt, model + reasoning-effort pickers, attachment staging
  Sidebar.svelte / SidebarItem.svelte
  SettingsSheet.svelte      server settings overlay (forced open until setup is complete)
  LoginScreen.svelte        server address (native) + credentials, one screen for both gates
  AttachmentViewer.svelte   fullscreen text/image lightbox + gallery
  ImageGallery.svelte       capped square-thumbnail grid for message images
  AttachmentImage.svelte    <img> for an image attachment: local preview until the server copy is decoded
  Scramble.svelte           crush-style waiting cursor (pending send + live generation)
  PanelSheet.svelte         resizable side sheet (Logs / Tools / System prompt)
  ModelPickerSheet.svelte   mobile bottom drawer for model + reasoning effort
  AttachmentSheet.svelte    mobile bottom drawer for camera / photos / files
  ConfirmModal.svelte / ConfirmSheet.svelte   centered dialog (desktop) / bottom sheet (mobile)
  ThinkingBlock.svelte, ToolCallItem.svelte, CollapsibleStatus.svelte
  GenerationError.svelte, CopyButton.svelte, IconButton.svelte,
  ResizeHandle.svelte, Spinner.svelte
```

## App shell (App.svelte)

- **Routing**: hash-based, two routes — `#/` (home) and `#/c/:chatId`. Route changes call `app.openChat(id)` / `app.closeChat()`. An auth-gated `$effect` applies the route only once auth is resolved.
- **Deep links vs. restored tabs**: a per-tab `sessionStorage['chattoneko-booted']` flag tells an explicit deep link (fresh tab → honour `#/c/<id>`) apart from a browser-restored hash (reload / session restore → land on the new-chat screen, never auto-restore the last chat).
- **Theme**: seeded before first paint by an inline script in `index.html` (no flash): `localStorage['chattoneko-theme']` or the system preference, dark when the system reports no light preference. `theme.svelte.js` keeps it in sync afterwards, follows live system changes until the user picks explicitly, and (native only) matches the Android status-bar style/background to the theme.
- **Boot gates** (in order): auth check in flight → spinner; native build with no configured server, or auth enabled and not signed in → `LoginScreen` (native adds the server-address phase on top of the same card); server unreachable → server-down card (with "Change server" on native); otherwise the main layout.
- **Layout**: collapsible/resizable sidebar + chat column. Desktop (≥lg): inline sidebar with a drag handle (`ResizeHandle`, persisted to `localStorage['chattoneko-sidebar-width']`, 200–480px, default 288), a persisted collapse state (`chattoneko-sidebar-collapsed`) and a width-only slide transition that overrides `min-width` while it runs. Mobile: the sidebar lives in a fullscreen left `Sheet` opened from the header. The header's menu button is routed via a window-level listener on `[data-sidebar="trigger"]`.
- **Android back button** (native only): `@capacitor/app`'s `backButton` listener closes the change-server screen first, then the topmost entry of the overlay registry (`lib/overlays.svelte.js`), then walks hash history, then exits the app. Every sheet/dialog/popover registers its close callback on mount.

## State store (lib/state.svelte.js)

A single reactive class instance (`app`) exported module-wide holds everything: auth state, server config, config payload (`/api/config`), chat list + pagination + search, the active chat + messages, drafts, staged attachments, live-generation state, and the SSE streams. Components import it directly and read/write via runes; there is no other state library.

### Boot & auth (`init`)

1. Detect native (`isNative()` → `window.Capacitor.isNativePlatform()`). Native with no stored server URL → `needsServerSetup`, stop.
2. `GET /api/meta` → `auth_enabled` + `setup_complete`. Failure → `serverDown`.
3. Auth check: disabled → authed; enabled → a missing/expired stored JWT (checked client-side via its `exp` claim) is dropped and lands on the login screen; otherwise `GET /api/auth/me` with the Bearer token.
4. When authed: attach the global + title streams, then load config and chats in parallel.

Login: one path for web and native — `POST /api/auth/login` returns a signed JWT (90-day TTL), persisted as `localStorage['chattoneko-token']` and sent as `Authorization: Bearer <token>` on every request (EventSource streams and attachment `<img>` loads append `?token=`). The password is never stored. Logout just discards the token (JWTs are stateless — nothing to invalidate server-side) and reloads. A 401 anywhere clears the token, closes every stream (their URLs carry the dead token and cannot re-authenticate) and drops to the login screen with a toast.

### Server configuration (native)

`setServer(url)` persists the address and re-runs `init`; `confirmServer(url)` persists it and stays on the screen so the credential fields can appear under the locked address; `changeServer()` / `cancelChangeServer()` drive the deliberate switch, which never clears anything until confirmed. URLs are normalized (default `http://` scheme, trailing path junk dropped) and probed with an unauthenticated `GET <server>/api/meta` before being accepted.

### Chats, drafts, sending

- **Chat list**: cursor-paginated from `/api/chats`; each entry carries a live `generating` flag. Search (sidebar input, 250ms debounce) hits `GET /api/chats?q=`; clearing restores the recents list.
- **Drafts** are stored in the store keyed by chat id (`""` = not-yet-created chat), so they survive chat switches and the Composer remount that first-send triggers.
- **Staged attachments** are client-side only (File objects + local preview URLs), keyed the same way and validated against the server's limits from `/api/config` — nothing is uploaded and no chat is created until send. On native they can come from the camera, the photo gallery or the file picker (`lib/native-attachments.js`), all of which return plain `File[]`; `addAttachments()` revalidates by extension regardless of the source.
- **`send(content, attachments)`**: sets `app.outgoing` (the content + staged attachments) so the message shows instantly, then `ensureChat()` — creates the chat server-side (`POST /chats`) on the first send, carrying the composer's model, reasoning effort and per-chat tool overrides; the returned `created` flag lets the failure path roll the empty chat back (delete it) so it never lingers in the sidebar. Then uploads attachments (recording server id → local preview URL in `app.previews`), sends the message, pushes the returned user message + an assistant placeholder, starts the live state, `kick()`s the chat stream so the new generation is picked up immediately instead of on the idle poll, and clears `outgoing` in `finally`.
- **Pending send (`app.outgoing`)**: over a slow link the create-chat, upload and send round trips would otherwise leave the chat empty until the POST response. `MessageList` renders `outgoing` as the tail row (a normal user bubble with the staged previews, plus a `Scramble` under it) while `outgoing && !live`; the real row arrives through the normal paths (POST response or the SSE `user_message` broadcast) and the pending one just disappears. It is deliberately NOT in `messages`, so nothing that reloads the list (`refreshChat`, the `idle` a fresh stream subscribe emits) can wipe it and there are no ids to reconcile. No fake Thinking pill: that appears only when the server's placeholder does. The `Scramble` is continuous from the keypress to the last token — `MessageItem` shows it for any live generating reply, including under an empty Thinking pill.
- **Image previews across the swap (`AttachmentImage.svelte`)**: a staged image renders its local object URL (`att.previewUrl`). When the real row replaces the pending one, its fresh `<img>` would paint the server copy progressively over a slow link — a picture already on screen visibly re-drawing. So `AttachmentImage` starts on the local copy looked up by server id (`app.previewFor`), preloads the server URL with `Image.decode()`, and flips `src` only once decoded (browser cache + decoded bitmap = one paint), then revokes the object URL and forgets the entry. Persisted images with no preview go straight to the server URL. `ImageGallery` renders through it on both sides; `AttachmentViewer` keeps `previewUrl || server` (user-initiated lightbox).

### Live generation state

While a generation runs, `app.live` accumulates stream events for the active assistant message: `display` / `reasoningDisplay` text, `toolCalls` (start → argument deltas → final args → result), `attachments` (tool-created files), `status`, `error`. `MessageItem` renders live messages PURELY from stream events; terminal messages render from the REST-persisted fields — a reconnect mid-generation simply replays the buffered events and re-materializes the same view. On `done`, live state is merged into the message and cleared.

### SSE streams (lib/stream.svelte.js)

- **`ChatStream`** (per open chat) implements the resume contract: connects to `/api/chats/{id}/stream?after=<lastSeq>`, dedupes by `seq`, resets the dedupe baseline when the event `epoch` changes (server hub recreated → seq space restarts), reconnects 1s after unexpected drops and on a slow 4s poll after clean closes (`idle`/`done` — this is how generations started from OTHER clients get picked up), and exposes `kick()` for immediate reconnect after a local POST.
- **`GlobalStream`** (`/api/stream`) carries cross-chat lifecycle events for the sidebar: `generating_snapshot` on connect (full server truth), then `generation_started` / `done` / `chat_updated`, plus `config_changed` (the MCP tool catalog was rebuilt after a settings save — the store refetches `/api/config`). The store maintains `chatGeneratingIds` (a `SvelteSet`, so add/delete are reactive) — the set of background chats with a running generation, rendered as breathing sidebar titles and reconciled against the chat-list flags on load/focus. The active chat is excluded (its own stream owns it).
- **`TitleStream`** (`/api/stream/titles`) applies final auto-generated titles to the sidebar entry and the open chat's header. Global and title streams rely on EventSource's built-in reconnect for closed sockets; only ChatStream needs manual retry logic.
- **Stall watchdog** (`openStream`, shared by all three): EventSource only fires `onerror` on a CLOSED socket. A half-open one (phone switched networks, NAT mapping expired) looks open forever while the reply and its `done` land in a dead pipe — the thinking pill and the breathing title would spin until a reload. The server pings every 20s as a real `{type: "ping"}` event; 60s with nothing at all closes the EventSource and reconnects (ChatStream via `kick()`, so `?after=<lastSeq>` replays what was missed, or gets `idle` → `refreshChat` once the grace period is over). Pings never reach the event handlers.

Event vocabulary handled by the store: `idle`, `generation_started`, `delta`, `reasoning_delta`, `tool_call_started`, `tool_call_delta`, `tool_call_done`, `tool_result`, `attachment_created`, `status`, `done`, `user_message` (sent from another client), `chat_updated`, `settings_updated` (per-chat settings changed elsewhere — applied without refetch), `messages_reset` (history truncated elsewhere — refetch the chat), `config_changed` (MCP catalog rebuilt — refetch `/api/config`).

## Markdown

Two modules keep the heavy third-party stack off the critical path:

- `markdown.impl.js` — the real pipeline: [incremark-renderer](https://github.com/qingyunwy/incremark-renderer), a marked-based renderer built for progressively arriving markdown. It splits the source into stable blocks plus a mutable tail, re-lexes only what changed, and patches block-level DOM nodes in place, so a growing message never re-parses or repaints the whole document. Math runs through KaTeX in MathML mode — native `<math>` in the browser's system math fonts, no webfonts and no KaTeX stylesheet — and `$$..$$` / `$..$` / `\[..\]` / `\(..\)` are all handled natively, including mid-stream, so no delimiter pre-normalization is needed. The module owns the three places where incremark's defaults are bent back to this app's markup: `marked.breaks` (single newline → `<br>`), `highlight.renderBlock` (emit the app's own `<pre class="codeblock">` rather than incremark's `.incremark-code-block` wrapper, so the theme CSS and the copy button keep working, and drop the trailing newline incremark always appends to code text), and `sanitizeHtml.sanitizer` (incremark's xss filter, then `target="_blank" rel="noopener noreferrer"` on every link — marked's default renderer emits no target attribute, and `marked.renderer` via `setOptions` would clobber incremark's own renderer wholesale). `container: false` because models don't emit `:::` and a stray one should stay literal. Reached ONLY through the dynamic `import()` in `markdown.js`; a static import anywhere in the reachable graph merges it back into the entry chunk.
- `markdown.js` — the façade every call site uses, plus the pure string helpers (zero deps, entry chunk). `loadMarkdown()` is idempotent (one promise, one request), `onMarkdownReady(fn)` lets a component `$effect` re-run when the chunk lands, and `createRenderer(el)` binds an incremental renderer to an element — returning `null` (and self-triggering the load) until the chunk arrives, so the caller paints an escaped `whitespace-pre-wrap` span first and upgrades in place a moment later. Nothing renders blank.
- `normalizeHeadings(src)` splits a heading a model glued onto the end of a sentence (`"…power laws.# Power Law"`) onto its own line; without it marked renders the `#` as literal text. The punctuation-before / capital-after requirements keep `C#`, `example.md#anchor` and `#hashtag` intact. `splitHeadingHold(carry, delta)` is the streaming form: a delta can cut that pattern anywhere, so it holds back a trailing fragment that could still become a heading and releases it once the next delta settles it — a few characters at most, flushed by the caller at end of stream. It has no code-fence awareness (see the `ponytail:` note in the source).

## Rendering

- **MessageList** builds display items in two passes: pass 1 collects `role=tool` messages into a `tool_call_id` map (they persist AFTER the assistant message that requested them, so a single forward pass would leave calls pending forever), pass 2 folds them into the preceding assistant message's tool calls by provider call id; tool messages never render standalone. The live assistant placeholder gets the live tool calls. Auto-scroll is "pinned follows the stream" — any deliberate upward scroll unpins instantly; a jump-to-bottom button re-pins.
- **MessageItem** streaming: one incremental renderer per message element, fed the typewriter's growth as deltas. A single `$effect` reads `content` / `contentEl` / `mdReady` / `isLive` and drives it. While live it appends `content.slice(seen)` through `splitHeadingHold`, so only incremark's mutable tail is re-lexed per frame; if the content no longer extends what was already fed (regenerate, edit, chat switch) the renderer is reset first. When the message goes terminal with everything already fed it flushes the holdback and calls `finalize()` — freezing the tail rather than re-parsing the message; a historical message that was never streamed goes through `setMarkdown(normalizeHeadings(content))` in one pass. Svelte owns the `<div class="md-body">`, the renderer owns everything inside it. The per-code-block copy button is re-created after every patch, because replacing the streaming block node takes the button with it. Tool-created files render as download chips only after `done` (they don't pop in mid-stream). Failed generations show `GenerationError`; stopped/failed states keep partial output visible.
- **Image descriptions (vision model).** When the chat model can't see images, the server substitutes a vision-model text description in the outgoing request, but the user still sees the original image. Image attachments that have `has_description` get a small eye badge on the thumbnail; tapping it opens a popover that lazy-fetches and shows the description the model actually received (`GET /api/attachments/{id}/description`).
- **Image gallery (`ImageGallery.svelte`).** Image attachments on BOTH sides render as a capped grid of square thumbnails: a lone picture keeps its natural aspect ratio uncropped; exactly 2 or 3 get one cell each so a phone never shows an orphaned last-row cell; 4+ fill a 2-column grid that reflows to 3 via CONTAINER queries once the chat column is wide enough (the column is user-resizable, so viewport breakpoints would lie). Past `cap` (6) the last cell darkens into a "+N" tile. Every cell opens the lightbox AT that image with the whole set attached (`viewer.open(att, items)`), so the hidden ones are one swipe away. The right-aligned user bubble is shrink-to-fit — an fr-track grid inside a fit-content block collapses to min-content — so the grid there carries an explicit `widthClass`; the "what the model saw" eye badge rides along as a per-cell `overlay` snippet rendered as a SIBLING of the thumbnail button, keeping its clicks out of the viewer's.
- **Attachment viewer (`AttachmentViewer.svelte`, state in `lib/viewer.svelte.js`).** Clicking any attachment rendered in a message — user-uploaded images/text chips and tool-created `show_image` / `create_text_file` files — opens a fullscreen lightbox instead of navigating to the raw URL (`viewer.open(att)`; `App.svelte` mounts the overlay once, deliberately unkeyed so navigation reuses the mounted dialog). It is a native `<dialog>` (top layer, Escape + focus trap for free). Text attachments are fetched through `api.attachmentText()` and shown as inert soft-wrapped monospace with copy + download; images get a pointer-event pan/zoom stage: pinch on touch, wheel + drag on desktop, double tap/click toggles 100%↔250%, tap on the dark area around the picture dismisses. The +/− zoom buttons and percentage reset are `pointer:fine`-only — touch relies on the gestures. Opened from a multi-image set the SAME overlay becomes a gallery: a "3 / 12" counter joins the header subtitle, side chevrons (`pointer:fine`-only) and ←/→ keys step through the set, and on touch a horizontal flick at 100% zoom swipes between pictures — zoomed in, the same drag still pans. Navigation wraps at both ends (`viewer.step` via `prev()`/`next()`), zoom/offsets/load state reset per picture, and the two neighbours are preloaded so stepping feels instant. The download control stays a plain `<a href download>` to `api.attachmentUrl(id)`. Deliberate exception to the semantic-token rule: the overlay is black in both themes (media-viewer convention), so its chrome uses raw white-on-black.
- **ChatHeader**: chat title (editable), token totals + context-window usage from `/api/config` model_info (desktop), a new-chat button (mobile), and a 3-dot popover menu at EVERY size holding theme toggle, Logs, Tools, System prompt, Settings, Change server (native) and Log out. The three `PanelSheet` panels are Logs (plain-text conversation dump with copy), Tools (per-tool enable/disable toggles for the chat — persisted in `chats.tools_json` and re-hydrated into `app.chatToolOverrides` on open, so they survive chat switches and reloads; each tool starts from its `default_enabled`) and System (the effective system prompt, read-only). `PanelSheet` owns its resizable width (`chattoneko-logs-width` / `-tools-width` / `-system-width`) and is fullscreen below `sm`.
- **SettingsSheet**: the server settings overlay — provider base URL + API key (masked, revealed by an eye icon, stored value sent back as-is), the model whitelist as cards (chat/task/vision role flags, per-model metadata: modalities, context length, reasoning efforts + default, "fetch data" from the provider's `/models`), MCP servers (transport, command/args or url/headers, default_enabled), **Tool defaults** (config `tool_defaults`: a sparse map of explicit overrides over the live catalog `app.config.tools`, minus the tools of any MCP server whose row was removed or renamed in the unsaved form, so the list follows form edits immediately; a newly added server's tools appear only after the save dials it), the system prompt and the limits. It reads `GET /api/setup` and saves a partial `PUT /api/setup`, then `app.refreshAfterSetup()`. **Forced-open contract**: while the server reports `setup_complete === false` it opens over everything and cannot be dismissed — no close button, backdrop clicks and Escape ignored — until a save completes the config.
- **Composer**: auto-growing textarea, model picker + reasoning-effort picker (levels from model_info; for an existing chat patching persists to the chat, for a new chat it seeds creation), attachment button with an extension whitelist, clipboard paste → attachment staging, stop/regenerate buttons while generating. Desktop uses a popover `Select`; below `sm` a single centered model chip opens `ModelPickerSheet` (a vaul bottom drawer with a segmented effort control and large tappable model rows — popover select targets are too small for touch). On native the attachment button opens `AttachmentSheet` (camera / photos / files) instead of a file input. When staged images target a model whose `input_modality` lacks `image`, a small eye-icon hint line explains the images will be described by the vision model (or that none is configured).
- **Sidebar**: new chat, refresh-from-database (header button on ≥sm, pull-to-refresh on the list below it), debounced search, per-item hover actions (rename inline, delete via `ConfirmModal` on desktop / `ConfirmSheet` on mobile), breathing animation for chats generating in the background.

## REST client (lib/api.js)

Thin `fetch` wrapper centralizing every endpoint + response normalizer (contract drift is a one-file fix). Base URL: same-origin `/api` on the web; `<server>/api` when a server is configured (native). Request defaults: `Authorization: Bearer <token>` whenever a JWT is stored (web and native alike); no cookies. Any 401 clears the token and triggers the store's unauthorized handler (close streams, drop to login screen with a toast). Includes `probeServer` (unauthenticated `GET /api/meta` used by the login screen's address phase).

## Styling notes (app.css)

- Theme tokens for light and dark, plus custom additions to the shadcn set: `--warning`, `--success`, the `--sidebar-*` family and a code-block surface token. All exposed to Tailwind through `@theme inline`.
- System font stacks only — no webfont downloads.
- Content text is 15px everywhere (assistant markdown, user bubbles), scaled up on `max-width: 640px` / `pointer: coarse` to match native chat apps.
- `.p-safe` / `.p-safe-pad` apply Android edge-to-edge / notch safe-area insets; `capacitor.config.json` disables Capacitor's own margin adjustment so the CSS owns it.
- `::selection` uses `--primary` rather than `--accent` because user message bubbles are `bg-accent`.
- Universal press feedback and Tailwind v4's restored pointer cursor on buttons/summaries live OUTSIDE `@layer base` — inside it they lose to every Tailwind `bg-*` utility.
- Math needs no stylesheet: KaTeX runs in MathML mode, so formulas are native `<math>` in the browser's own system math fonts. Only the highlight.js token palettes are inlined here.
- incremark wraps every block in a `<div data-incremark-block>`, so the inter-block gap is declared twice: `.md-body > * + *` (between wrappers) and `.md-body > [data-incremark-block] > * + *` (between the elements one wrapper can hold). Margins collapse through the wrapper — it has no border or padding — which keeps the result identical to the old unwrapped layout, and a heading's own 1.1em still wins over the 0.65em sibling gap.
- `.incremark-math-block` is the horizontal-scroll wrapper for display math (`overflow-x` on `<math>` itself is ignored by Firefox, which would push a scrollbar onto the whole chat). It deliberately carries NO vertical margin: a scroll container is a block formatting context, so its margins cannot collapse with the `[data-incremark-block]` wrapper around it and the two gaps would stack.

## Local persistence

`localStorage` only, all keys `chattoneko-` prefixed: theme, sidebar collapsed + width, the three panel widths, the server URL (native only) and the JWT auth token. `sessionStorage['chattoneko-booted']` is the per-tab route-restore flag. Everything else — chats, messages, files — lives on the server.
