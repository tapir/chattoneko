# Backend Architecture

Chattoneko is a single Go binary: embedded Svelte SPA + REST/SSE API + SQLite storage + one OpenAI-compatible provider + tools (integrated built-ins and MCP servers).

```
main.go                      startup wiring, flags, graceful shutdown
internal/
  config/                    SQLite-backed config store (live snapshots, updates, model metadata)
  db/                        SQLite open, embedded schema migration
  db/query/                  sqlc-generated typed queries (queries.sql)
  store/                     persistence layer over db/query (domain types)
  provider/                  normalized OpenAI-compatible streaming client (live-swappable endpoint + /models fetch)
  mcphub/                    MCP server connections (stdio + http), live-reloadable
  tools/                     integrated tools + merged tool catalog
  engine/                    generation turn loop, per-chat SSE hubs
  titlegen/                  background chat-title task + its own SSE hub
  vision/                    image descriptions for chat models without image input
  attach/                    upload processing (image → PNG, EXIF orientation, text sniffing)
  webimage/                  bot-detection-resistant image URL fetching (show_image tool)
  auth/                      optional single-user JWT auth (env-var driven, plaintext password)
  api/                       HTTP routes, SSE endpoints, SPA serving, CORS
```

## Startup sequence (main.go)

1. Parse flags: `-db <path>` (default `chatto.db` next to the executable, so the same DB is used regardless of the working directory), `-listen <host:port>` (HTTP listen address, fixed for the process lifetime; default `:8080`), `-debug` (debug logging). There is no config file; everything except the listen address (flag) and single-user auth (env vars) lives in the database.
2. Configure structured logging (`slog`, text handler on stderr; `-debug` lowers the level to Debug).
3. Create the DB file with mode `0600` if missing (chats, messages and attachments live in it), open SQLite (pure-Go `modernc.org/sqlite`, `CGO_ENABLED=0` builds), run the embedded migrations.
4. `config.NewStore` loads (or seeds on first run) the configuration from the `config` table. Auth is not in the table: it is derived from the `CHATTO_USERNAME` / `CHATTO_PASSWORD` env vars (login required exactly when both are set). `warnIfExposed` logs a warning whenever auth is disabled and the listen address is not loopback-only.
5. Startup sweeps on the store: delete orphan attachments older than 24h (uploaded but never linked to a message), delete empty chats older than 24h (created but never messaged).
6. Create the live provider wrapper (`provider.NewLive` — starts unconfigured if the endpoint isn't set yet; fail-on-call, not fail-on-start), connect the MCP hub (dead servers are skipped with a warning), and merge the tool catalog: integrated tools first, then MCP tools; integrated tools win display-name collisions. The merged catalog is computed lazily on every call so live changes are visible without rebuilding it. `tools.WithDefaults` wraps it with the configured global per-tool defaults (`tool_defaults`), so the engine and the API both see the settings-UI default for every tool.
7. Create the engine on a server-scoped context — generations survive HTTP client disconnects and end only via stop, chat deletion, or server shutdown. Recover crashed generations left behind by an interrupted process (see engine section).
8. Start the title task (its own goroutine + independent SSE hub).
9. Assemble the API server with the embedded `web/dist` filesystem, bind the `-listen` address (a bind failure is fatal) and serve. The `http.Server` sets `ReadHeaderTimeout` only — no `WriteTimeout`/`ReadTimeout`, which would kill SSE.
10. Graceful shutdown on SIGINT/SIGTERM: stop the HTTP listener (10s), then `engine.Shutdown()` (cancels active generations and waits for their final persistence), close MCP sessions (kills stdio child processes), close the DB, cancel the server context.

**Live config wiring.** `cfgStore.Subscribe` registers the reactive components once at startup: `prov.Reconfigure` re-dials the provider when base_url/api_key change, `warnIfExposed` re-checks the open-API combination, and `hub.Reload` connects/closes/reconnects MCP servers to match the new list. The reload runs async (MCP dials can take real time) and, when it changed the tool catalog, the engine publishes a `config_changed` event on the global stream so clients refetch `/api/config` — the setup-save response goes out before the dial finishes. Engine, API, auth and the title task all read the current snapshot per request/sweep, so system prompt, limits, models, and credentials change with no restart. The listen address is NOT live-editable: it is the `-listen` flag, bound once at startup.

## Configuration (internal/config)

All configuration lives in SQLite — there is no config file and no environment variables for it. Two tables (migration `001_init.sql`): `config(key, value, updated_at)` for global settings (structured values are JSON) and `models(model_id, input_modality, output_modality, context_length, reasoning_efforts, reasoning_default, updated_at)` for per-model metadata.

`Store` is the live handle. On first run (config table completely empty) it seeds the defaults: `system_prompt` from the embedded `internal/config/default_system.md`, `upload_max_file_bytes = 5242880`, `max_tool_iterations = 10`, `mcp_call_timeout_seconds = 60` — everything else (provider endpoint/key, model whitelist + defaults, MCP servers, tool defaults) stays empty until configured through the API. Auth is never seeded or stored: it is sourced from the environment on every load (see Auth). `Get()` returns the current immutable snapshot; `Update(ctx, Patch)` applies a partial patch transactionally, finalizes (empty/invalid values fall back to defaults, broken MCP entries and empty/duplicate whitelist entries are dropped with a warning), validates, writes the full row set in ONE transaction, swaps the snapshot atomically and notifies subscribers. `Subscribe(fn)` is how every component gets live updates (see Startup/wiring). `Complete()` reports whether provider base_url/api_key and both designated models are set — the "setup still needed" flag exposed as `setup_complete` in `/api/meta`.

Per-model metadata (`ModelMeta`): input/output modality (subset of text|image|video|audio, default `["text"]`), context length (default 131072), reasoning-effort levels (default `["low","medium","high"]`) and the default effort (default: the 2nd element). `POST /api/setup/models` fetches these from the provider's `/models` endpoint for a list of model ids and stores them; fields the provider doesn't report fall back to the defaults above, and ids the provider doesn't list at all get pure defaults. Reads for ids without a stored row also return defaults.

The provider/model fields are not required for startup: a fresh server comes up in setup mode and chat attempts fail cleanly ("provider is not configured yet" / "no chat model configured yet") until the setup API fills them in.

The config snapshot carries: `system_prompt`, `provider` (base_url, api_key), `models` (whitelist, default_chat_model, default_task_model, default_vision_model), `mcp_servers` (name, transport stdio/http, command/args or url/headers, default_enabled), `limits`, `tool_defaults` (tool name → default enabled; the global per-tool default the settings UI edits, overriding the catalog's own default for tools it lists), `auth` (enabled, username, password — all sourced from the `CHATTO_USERNAME` / `CHATTO_PASSWORD` env vars, never persisted). The listen address lives outside the snapshot (CLI flag); a designated model that is not whitelisted is cleared by `sanitizeWhitelist`. The vision model is optional: `Complete()` does not require it, and when it is empty images are sent to the chat model as-is.

## Storage (internal/db, internal/store)

SQLite via the pure-Go `modernc.org/sqlite` driver, serialized access through `database/sql`. Schema migrations are embedded (`db/migrations/*.sql`), applied at startup in lexical order, each inside one transaction together with its `schema_migrations` record (a mid-file failure rolls back the whole file; SQLite supports transactional DDL).

Schema (embedded migration `migrations/001_init.sql`). Every column is `NOT NULL` with a default so no query ever binds NULL:

- `config` — key/value global settings (`system_prompt`, `provider_base_url`, `provider_api_key`, `model_whitelist` (JSON), `default_chat_model`, `default_task_model`, `default_vision_model`, `mcp_servers` (JSON), `upload_max_file_bytes`, `max_tool_iterations`, `mcp_call_timeout_seconds`, `tool_defaults` (JSON)), each with `updated_at`. Owned exclusively by `internal/config.Store`. (The listen address is NOT stored here — it is the `-listen` CLI flag. Auth is NOT stored here either — it is env-var driven.)
- `models` — per-model metadata keyed by `model_id`: `input_modality` / `output_modality` (JSON arrays of text|image|video|audio), `context_length`, `reasoning_efforts` (JSON array), `reasoning_default`. Owned by `internal/config.Store` (ModelMeta methods).
- `chats` — id (uuid), title, `title_generated` (1 = title is final: auto-title ran or the user renamed manually), model, `params_json` (`{"reasoning_effort": "..."}`), `tools_json` (per-tool enable/disable overrides of config defaults), created_at/updated_at (unix millis). Indexed by `updated_at DESC, id DESC` for sidebar pagination.
- `messages` — `seq` (global monotonic autoincrement, the ordering key), id (uuid), chat_id (FK, cascade delete), role (`user | assistant | tool`), status (`complete | generating | stopped | failed`), content, reasoning, error, `tool_call_id` + `name` (role=tool), model (which model produced this assistant message), `prompt_tokens`/`completion_tokens`/`duration_ms` (per-turn usage), timestamps. Indexed by `(chat_id, seq)` plus a PARTIAL index on `seq WHERE status = 'generating'` so crash recovery never scans the message table.
- `tool_calls` — id, message_id (FK), provider_call_id, name, arguments (JSON), position.
- `attachments` — id, chat_id (FK), message_id (`''` = orphan until linked at send time), filename, kind (`image | text`), mime, size (original upload size), data (image: re-encoded PNG; text: raw UTF-8), description (cached vision-model text description of an image; `''` until generated), created_at. Indexed by message_id and by chat_id (chat load lists every meta; truncation reaps danglers).

`store.Store` wraps the sqlc-generated `query` package and is the only persistence interface the rest of the server uses. It exposes chat CRUD + cursor pagination + text search, message CRUD ordered by `seq`, per-message usage, chat token totals, tool-call records (including `ListDanglingToolCalls` — calls without results, used by crash recovery), attachments (create / link-at-send / orphan + dangling cleanup), title bookkeeping (`ListChatsNeedingTitle`, conditional `SetGeneratedTitle`, `MarkTitleGenerated`).

## Generation engine (internal/engine)

The engine owns every in-flight generation. Key design points:

- **Server-scoped context.** A generation runs on a child of the server context, not the HTTP request context: client disconnects never abort a generation. It ends only via user stop, chat deletion, or server shutdown.
- **One generation per chat.** Each chat has a `chatHub` holding at most one active generation. Handlers claim the generation slot BEFORE persisting anything user-visible (`ClaimGeneration` → persist → `StartClaimedGeneration`), so a concurrent request loses the claim up front with HTTP 409 instead of leaving an unanswered user message behind.
- **Claim/release lifecycle.** `StartGeneration` creates the assistant message (status `generating`, stamped with the model in effect at creation — the chat model can change mid-conversation) and spawns `runGeneration`. `StopGeneration` marks the generation stopped and cancels it (partial output kept). `CancelForChatDeletion` aborts without further persistence. `Shutdown` cancels everything and waits (5s cap) for final persistence so the DB close can't kill a write.

### Turn loop (runGeneration)

1. Load the chat's persisted settings → `provider.GenParams` (model, reasoning effort; model falls back to `default_chat_model`).
2. Loop: rebuild the full message list from the DB every iteration (system prompt first, then history with attachments), compute the effective tool set, stream one provider completion.
3. Drain the provider stream, publishing wire events as they happen (`delta`, `reasoning_delta`, `tool_call_started/delta/done`) and accumulating text/reasoning into the active generation. Usage tokens are accumulated across iterations; cancellation (stop) is honored promptly mid-stream.
4. If the turn produced tool calls: persist accumulated content + the calls first (crash safety), execute each call (`executeTools`), persist each tool result as a `role=tool` message, publish `tool_result`, announce tool-generated files (`attachment_created`), then iterate. A disabled-by-user tool yields an explicit error result instead of executing. The loop is bounded by `limits.max_tool_iterations` (default 10); reaching the cap finalizes cleanly (every call below it has a real result).
5. If no tool calls: finish. A truncation finish reason (`length`, `max_tokens`, `max_output_tokens`, `output_length`, `model_max_tokens`) is surfaced as a FAILED generation with a clear error instead of silently keeping a cut-off answer.

Incremental persistence: a flusher goroutine persists accumulated text/reasoning every 500ms while dirty. Finalization (`finish`) stops the flusher FIRST and waits for any in-flight flush (a stale snapshot must never overwrite final content), then — on a cancellation-detached context — synthesizes results for dangling tool calls, writes the terminal status, persists usage/duration, touches the chat, and publishes `status` + `done` (with usage) to the hub. The replay buffer is kept for a 5s grace period for late subscribers, then the hub reference is dropped and the hub pruned when nothing references it.

**Tool-call history invariant:** readers must never see a terminal assistant message with unanswered tool calls. Every exit path (complete, stopped, failed, deleted-crash) synthesizes `role=tool` results for dangling calls before finalizing.

**Crash recovery:** on startup, `RecoverCrashed` finds messages still marked `generating`, synthesizes tool results ("server restart interrupted generation"), and finalizes them as `failed` with "interrupted (server restart)".

### Effective tools

Sent to the provider per turn: enabled tools (chat's `tools_json` overrides ∪ config defaults) PLUS any tool referenced anywhere in the chat's history. The config default per tool is `tool_defaults[name]` when the settings UI set one, else the catalog's own default (integrated tools: hardcoded `DefaultEnabled`; MCP tools: their server's `default_enabled`). Omitting a disabled tool whose calls exist in history would make `chat/completions` reject the request with orphan tool_call ids. Tool definitions go via the provider's native tools parameter; the system prompt carries the configured base prompt only.

### SSE fan-out (per-chat hubs)

Every chat hub keeps: a chat-scoped monotonic event `seq`, an `epoch` (hub incarnation id — seq restarts at 1 when a hub is pruned and recreated), subscriber channels (buffer 1024), and the current generation's replay buffer.

- Generation events (`publishGen`) are appended to the replay buffer with the next seq and delivered to all subscribers. A subscriber whose buffer is full is detached (channel closed) and must reconnect — the replay covers the gap.
- Chat-level events (`user_message`, `chat_updated`, `settings_updated`, `messages_reset`) go to current subscribers; `chat_updated` is additionally replayed during the grace period so a rename landing right after `done` is not lost to a reconnecting client.
- Subscribing (`Subscribe(chatID, after)`) replays every buffered event with `seq > after` losslessly; with no generation present, an `idle` event is emitted immediately.

**Global (all-chats) stream.** Sidebar-relevant lifecycle events — `generation_started`, `done`, `chat_updated` — are additionally fanned out to engine-level global subscribers so clients can track background generations (breathing titles) without attaching a stream to every chat; `config_changed` rides the same stream to tell clients the MCP tool catalog was rebuilt after a config save (refetch `/api/config`). On subscribe, clients first receive a `generating_snapshot` listing every chat with a running generation, so (re)connecting reconciles without polling.

Wire event vocabulary (`WireEvent`): `generation_started`, `delta`, `reasoning_delta`, `tool_call_started`, `tool_call_delta`, `tool_call_done`, `tool_result`, `attachment_created`, `status`, `done` (carries per-turn usage + duration), `idle`, `user_message`, `chat_updated`, `settings_updated`, `messages_reset` (history truncated elsewhere — refetch), `generating_snapshot`, `config_changed` (global).

## Provider (internal/provider)

Talks to exactly one OpenAI-compatible endpoint via `POST /chat/completions` (official `openai-go` SDK). `types.go` defines the normalized wire contract — `Message` (role, content, images as PNG data URLs, tool calls, tool_call_id), `Tool`, `GenParams` (model + optional reasoning effort, sent only when non-empty), `Usage`, and an `EventStream` of normalized events (`EventTextDelta`, `EventReasoningDelta`, `EventToolCallStart/Delta/Done`, `EventError`, `EventDone` with finish reason + usage). `chat_completions.go` implements it: builds the wire messages/tools, requests streaming with `stream_options.include_usage` (terminal usage chunk), parses deltas into the normalized event kinds, and accumulates tool-call argument fragments. Both `base_url` and `api_key` are required — fail fast instead of letting the SDK fall back to its defaults. A stream that delivers no data at all for 5 minutes is aborted (idle timeout); nothing else bounds an in-flight stream.

`provider.Live` is the swappable wrapper the engine holds: `Reconfigure(baseURL, apiKey)` re-dials only when either value changed, and empty values leave it unconfigured so `StreamChat` returns `ErrNotConfigured` instead of dialing an empty URL.

## Model metadata (internal/config, models table)

Per-model metadata (modalities, context length, reasoning efforts) is stored in the `models` table and read live — no in-memory cache or resolver. `config.ModelMetas(ctx, ids)` returns one entry per id, substituting the spec defaults for ids without a row; `UpsertModelMetas` sanitizes (modality filter, positive context, default effort must be a listed level else falls back to the 2nd) and writes. `provider.FetchModels` pulls the provider's `/models` endpoint (15s timeout; OpenRouter-style fields: `context_length`, `architecture.input/output_modalities`, `reasoning.supported_efforts/default_effort`) and normalizes them; the `POST /api/setup/models` endpoint maps each requested id to provider data where available and defaults otherwise, then stores the result. `GET /api/config` exposes the stored metadata as `model_info`: one object per whitelisted model with `id`, `context_window`, `reasoning_efforts`, `reasoning_default`, `input_modality`, `output_modality`.

## Tools (internal/tools, internal/mcphub)

Two sources merged into one catalog (`tools.Merge`): integrated tools first (they win name collisions), then MCP tools. `tools.WithDefaults` wraps the merge with the config's `tool_defaults` map (tool name → default enabled, edited in the settings UI): listed tools report that default, unlisted ones keep their source's own. The wrapped catalog is the only tool interface the engine and API consume, so `GET /api/config` reports the same `default_enabled` the engine uses and a settings save applies live (the map is read per `Tools()` call).

Integrated tools (hardcoded, one file each; `tools.Builtin` lists them):

- `time_location` — returns the server's current local date/time/timezone (human-readable + RFC 3339), plus its location appended as ` — <location>` when the `CHATTO_LOCATION_STRING` env var is set (non-empty after trimming). Default-enabled.
- `simple_code` — runs a small Lua snippet in a restricted sandbox (Shopify/go-lua) and returns what it prints; an advanced calculator for exact arithmetic and data wrangling. The sandbox removes filesystem access and replaces `load()` with a text-only wrapper (no precompiled bytecode parsing).
- `create_text_file` — the model writes a complete UTF-8 text file which is stored as a chat attachment linked to the assistant message (5 MiB cap, same content validation as uploads) and surfaces as a download chip.
- `show_image` — the model passes a direct web URL of an image; `webimage.Fetch` downloads it in memory via bogdanfinn/tls-client impersonating a current Chrome (browser TLS/JA3/JA4 + HTTP/2 fingerprint, matching User-Agent and Chrome-typical headers with fixed order, shared cookie jar), then the SAME upload pipeline (`attach.Process`: sniff, re-encode to PNG, live `upload_max_file_bytes` cap with downscale fallbacks) stores it as an image attachment linked to the assistant message. The UI renders image attachments inline, so the user sees the picture instead of a link; HTML/bot-interstitial bodies are rejected in-band. Robustness details: the Accept list omits avif/svg so content-negotiating CDNs (imgix `auto=format`) serve WebP/JPEG our pipeline can decode; compressed bodies over HTTP/2 are decompressed manually (the transport only auto-unpacks on HTTP/1.1); MediaWiki thumbnails whose width the wiki rejects with HTTP 400 fall back once to the original file URL.

Every integrated call is bounded by a hardcoded 30s context timeout and wrapped in a panic recovery, so an in-process handler (including third-party VM code like the Lua sandbox) surfaces as an in-band tool error rather than killing the generation or the process. Handler errors are returned in-band (`isError=true`) exactly like MCP tool errors.

MCP hub (`mcphub`): connects every `mcp_servers` entry at startup (stdio via `exec.Command`, http via streamable transport with optional headers), 30s connect timeout per server; a dead server logs a warning and is skipped. Each connected session lists its tools, which become catalog entries with config-driven `default_enabled`. `Call` dispatches by display name to the owning source, bounded per call by `mcp_call_timeout_seconds`. `CallMeta` (chat id + owning assistant message id) lets integrated tools attach artifacts to the right message; MCP tools ignore it.

## Title generation (internal/titlegen)

Background task that is the ONLY writer of auto-generated titles. A polling loop (1s cadence, at most 16 chats per sweep) finds chats with `title_generated = 0` and picks the source from the first user message: its text (capped at 1000 runes) goes to `GenerateFromText`; a text-only first message goes to `GenerateFromFile` with the filename + content; an image-only first message gets the fixed title `User Image Input` with no model call. The model call uses a dedicated NON-streaming client (same provider endpoint, task model + its stored default effort, `max_output_tokens = 1024` so reasoning models can't spend the whole budget before emitting content, 30s per-call timeout). Transient failures retry with exponential backoff (2s → 30s cap) at most 5 times per chat, after which the chat keeps "New Chat". The result is capped at 60 runes and written conditionally (`SetGeneratedTitle` only while `title_generated = 0`) — a concurrent manual rename wins; the task then marks the title final either way.

Results are published on the task's own independent SSE hub (`/api/stream/titles`): separate mutex, channels and event type from the engine's machinery, so a stalled generation or slow chat subscriber can never delay or drop a title update. Title events are self-contained (chat id + title) and idempotent — no replay buffer needed.

## Attachments (internal/attach)

Upload processing with content-based validation (the extension is only a mime hint). Raw reads are capped at 64 MiB per file (`MaxRawUploadBytes`, also the constant the HTTP layer sizes its multipart ceiling from) and image headers are rejected above 12000px on a side before any pixel buffer is allocated. Decodable images (JPEG/PNG/GIF/WebP; GIF first frame) are re-encoded to PNG with the longest side capped at 2048px — `encodePNG` downscales and retries so a noisy photo whose PNG exceeds the cap is still accepted in compact form. Uploaded JPEGs have their EXIF orientation baked into the pixels first (`exif.go`): Go's JPEG decoder ignores EXIF and PNG carries none, so an uncorrected phone photo would arrive rotated everywhere downstream; a missing/malformed tag means "no transform". Valid UTF-8 text files are stored raw; anything else → HTTP 415. The per-file stored-size cap comes from `limits.upload_max_file_bytes` (HTTP 413). Filenames are sanitized.

Images are sent to the provider as `data:image/png` content parts when the chat model accepts images; otherwise they are replaced by a cached vision-model description (see below) serialized as a `<file name="..." id="...">` block. Text attachments are serialized into the user message as delimited `<file name="..." id="...">` blocks. Serving: images as `image/png`; text as `text/plain` regardless of detected mime (an uploaded HTML/SVG can never execute in the app's origin) with the real filename via `Content-Disposition`.

**Vision-model image descriptions (internal/vision).** Chat models whose `input_modality` lacks `image` cannot be sent image bytes. When such a chat receives a message with an image attachment, the engine asks the configured vision model (`models.default_vision_model`, same provider endpoint, dedicated NON-streaming client rebuilt lazily on config change — same pattern as the title task, 60s per-call timeout) for a detailed description, caches it on the attachment row (`attachments.description`), and injects it into the outgoing message as a `<file>` block under the image's own filename (`attach.SerializeImageDescription`). The description is generated lazily at build time and reused for every later generation, edit-resend, or regenerate; the stored PNG is untouched, so the user still sees the image. When no vision model is configured (or the call fails) the engine falls back to sending the raw image. The description is exposed to the UI via `GET /api/attachments/{id}/description` (404 until generated) and a `has_description` flag on attachment metas, so the frontend can show what the model received without altering the image the user sees.

## Auth (internal/auth)

Optional single-user auth, driven entirely by environment variables — there is no settings toggle. Login is required exactly when **both** `CHATTO_USERNAME` and `CHATTO_PASSWORD` are set (non-empty after trimming); if either is missing, auth is OFF and the app is fully open. The password is used as **plaintext** and is never written to the database. Auth is read once at startup; changing it means setting the env vars and restarting. When enabled:

- Login (`POST /api/auth/login`) verifies the credentials against the env-configured username/password and returns a signed JWT (HS256, 90-day TTL). Login attempts are rate-limited by an in-memory token bucket (5/min, burst 5), and both username and password are compared in constant time so a wrong username costs the same as a wrong password (no enumeration timing channel).
- The credentials live only in the process environment (`CHATTO_USERNAME` / `CHATTO_PASSWORD`) — they are never persisted to the config table and are not exposed through the setup API.
- The JWT signing key is derived from the credentials (`sha256("chattoneko-jwt:" + username + "\x00" + password)`), so tokens survive restarts and changing the env password invalidates every outstanding token. Tokens are stateless: no server-side session store, no logout invalidation — clients discard the token to log out.
- Requests are admitted with `Authorization: Bearer <token>`, or (GET only) `?token=` — EventSource streams and `<img>` attachment loads can't set headers. Only HS256 is accepted (no algorithm downgrade).
- `GET /api/meta` (auth_enabled + setup_complete flags) and login are public; every other `/api/*` route goes through the auth middleware. Expired tokens get a 401 and the clients (web + mobile) drop back to the login screen.

## HTTP surface (internal/api)

Route table (`ServeMux` with method patterns):

| Route | Handler |
| --- | --- |
| `GET /api/meta` | auth_enabled + setup_complete flags (public) |
| `POST /api/auth/login` | JWT login, returns `{username, token}` (public) |
| `GET /api/auth/me` | current username |
| `GET /api/config` | models whitelist + chat/vision defaults, model_info, tools catalog, effective system prompt, limits (`upload_max_file_bytes`, `max_tool_iterations`, `max_upload_files`, `max_raw_upload_bytes`) |
| `GET /api/setup` | full config, secrets included (`provider.api_key` and MCP header values as plain text so the settings UI can display and edit them; auth omitted entirely since it is env-var driven), plus per-model metadata (`models.metas`) for the whitelist + `complete` flag |
| `PUT /api/setup` | partial config update — only provided fields change (any `auth` field is ignored). `models.metas` upserts per-model metadata (models dropped from the whitelist lose theirs); `tool_defaults` replaces the whole global per-tool default map. Returns the full config. |
| `POST /api/setup/models` | accepts `{"model_ids":[...]}`; fetches the provider's `/models`, stores per-model metadata (defaults where unreported), returns the stored rows + per-id source |
| `GET /api/chats` | cursor-paginated chat list (`limit`, `before`, `before_id`) or `?q=` text search |
| `GET /api/stream` | global all-chats SSE stream |
| `GET /api/stream/titles` | title task's SSE stream |
| `POST /api/chats` | create empty chat ("New Chat"; optional model/params/tools) |
| `GET /api/chats/{id}` | chat + messages (seq-ordered) + `active` + effective system prompt + token totals |
| `GET /api/chats/{id}/log` | plain-text debug dump of the whole conversation |
| `PATCH /api/chats/{id}` | rename and/or per-chat settings; broadcasts `chat_updated` / `settings_updated` |
| `DELETE /api/chats/{id}` | cancels an active generation, deletes the chat (cascade) |
| `POST /api/chats/{id}/messages` | send: claim → persist user message (+ link attachments) → broadcast → start generation (409 if a generation is active) |
| `PATCH /api/chats/{id}/messages/{mid}` | edit a user message (optional attachment keep-list), truncate everything after it, re-generate; broadcasts `messages_reset` |
| `POST /api/chats/{id}/regenerate` | delete the last assistant message + everything after it (reap dangling attachments), re-generate; broadcasts `messages_reset` |
| `DELETE /api/chats/{id}/generation` | stop the active generation (partial output kept) |
| `GET /api/chats/{id}/stream?after=<seq>` | per-chat SSE stream with lossless replay |
| `POST /api/chats/{id}/attachments` | multipart upload, field `files` (≤8 files) |
| `GET /api/attachments/{id}` | stored bytes (image/png or text/plain download) |
| `GET /api/attachments/{id}/description` | cached vision-model text description of an image attachment (404 until generated) |

Cross-cutting:

- Every JSON response carries `Cache-Control: no-store` — config is live-editable, so a cached whitelist would outlive the change.
- JSON request bodies are capped at 1 MiB (`MaxBytesReader`, over-cap → 413, malformed → 400); multipart uploads have their own ceiling (raw per-file cap × files + 64 KiB overhead).
- SSE handler writes `event: message` + JSON `data` lines, flushes every event, and sends a `{"type":"ping"}` event every 20s. It is a real event, not an SSE comment, because EventSource never surfaces comments: the client uses the pings to detect a half-open socket (60s of silence → reconnect). Long-lived SSE paths are excluded from request logging.
- The claim → persist → start handlers (send, edit, regenerate) run their persistence on `context.WithoutCancel(r.Context())`: once the claim is taken, a client that disconnects mid-request (app killed on a slow link) must not strand a user message without a reply or a truncated history without its regeneration. The generation itself already runs on the server context.
- CORS middleware permits only the Capacitor WebView origins (`http://localhost`, `https://localhost`, `capacitor://localhost`) — the embedded web UI is same-origin and needs none; credentials stay off (the mobile app authenticates with Bearer tokens). Preflights are answered before routing/auth.
- SPA handler serves the embedded `web/dist`: hashed `assets/*` are immutable-cached; everything else is `no-cache`, with HTML falling back to `index.html` for client-side routes; unknown `/api/*` paths get a JSON 404.
- Request logging middleware records method/path/status/duration (5xx = error, 4xx = info, routine chat traffic = debug). The status recorder forwards `Flush` and `Unwrap` so SSE and `http.ResponseController` keep working through it.
