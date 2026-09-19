# AGENTS.md

ChattoNeko is a small self-hosted LLM chat app: one static Go binary with the Svelte SPA embedded, SQLite for storage, REST + SSE API, one OpenAI-compatible provider, built-in tools plus MCP servers. No accounts, no multi-tenancy, no config file (settings live in the DB and apply live).

Layout: `main.go` + `internal/` (Go backend), `web/` (Svelte 5 + Vite + Tailwind 4), `mobile/` (Capacitor Android wrapper around the same SPA), `ffmpeg/` (the static ffmpeg build that converts uploads), `docs/` (the real documentation).

## Read the docs

Details live in `docs/`, not here. Read the relevant one before changing that area:

- [docs/backend.md](docs/backend.md) — Go packages, DB/sqlc, config, provider streaming, engine turn loop, SSE hubs, tools/MCP, attach + media (upload classification, ffmpeg conversion), auth.
- [docs/frontend.md](docs/frontend.md) — Svelte runes, component tree, state stores, markdown pipeline, local shadcn-style UI kit and its editing rules.
- [docs/mobile.md](docs/mobile.md) — Capacitor setup, native bridge, build/sync, icons, emulator targets.

Also: [README.md](README.md) (user-facing overview, env vars, flags, tools).

## Commands

Everything goes through the `Makefile`:

- `make dev` — run the Go server (`go run . -db chatto.db`).
- `make build` / `make run` — build web + sqlc, then the binary (`make run` also starts it).
- `make web` — rebuild the frontend only; `cd web && npm run dev` for the Vite dev server (proxies `/api` to `:8080`).
- `make sqlc` — regenerate typed queries after editing `internal/db/queries.sql`.
- `make tidy`, `make docker`, `make ffmpeg` (the static ffmpeg the image ships; development runs any `ffmpeg` on PATH), `make mobile`, `make mobile-apk`, `make mobile-run`.

Go changes must be preceded by `make sqlc` when queries change, and the frontend must be built (`make web`) before `go build` since `web/dist` is embedded.

## Rules

- Never add explanatory/description text to the UI (hint lines, help paragraphs, "nothing to configure here" notes, extra sentences in existing notices) without asking first. Labels and placeholders only; copy comes from the user.

- Always use `testing` branch. `main` is protected against direct commits and `dev` is locked.