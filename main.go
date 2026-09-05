// Chattoneko — a self-hosted ChatGPT-style chat app. Single Go binary:
// embedded Svelte SPA + REST/SSE API + SQLite storage + one OpenAI-compatible
// provider + tools (integrated built-ins and MCP servers).
//
// There is NO config file. Every setting lives in the SQLite database
// (config + models tables) except two things: the listen address, which is
// the -listen CLI flag fixed at startup (default :8080), and single-user
// auth, which is driven by the CHATTO_USERNAME / CHATTO_PASSWORD
// environment variables — login is required exactly when BOTH are set, the
// password is used as plaintext, and neither is stored in the database. On
// the very first run the empty config table is seeded with defaults and the
// server comes up in "setup" mode until the provider/model fields are filled
// in through the API. Config edits apply live — see the subscriptions wired
// in run().
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"chattoneko/internal/api"
	"chattoneko/internal/auth"
	"chattoneko/internal/config"
	"chattoneko/internal/db"
	"chattoneko/internal/engine"
	"chattoneko/internal/mcphub"
	"chattoneko/internal/provider"
	"chattoneko/internal/store"
	"chattoneko/internal/titlegen"
	"chattoneko/internal/tools"
	"chattoneko/internal/vision"
)

//go:embed web/dist
var webFS embed.FS

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "chattoneko:", err)
		os.Exit(1)
	}
}

func run() error {
	dbPath := flag.String("db", "", "SQLite database file (default: chatto.db next to the executable)")
	listen := flag.String("listen", config.DefaultListen, "HTTP listen address, host:port — fixed for the process lifetime")
	debug := flag.Bool("debug", false, "enable debug logging (default level: info)")
	flag.Parse()

	// Database default: chatto.db BESIDE the binary, so the same database is
	// used regardless of the working directory chattoneko is launched from.
	dbFile := *dbPath
	if dbFile == "" {
		dbFile = "chatto.db" // fallback if the executable path cannot be resolved
		if exe, err := os.Executable(); err == nil {
			dbFile = filepath.Join(filepath.Dir(exe), "chatto.db")
		}
	}

	// One standard handler for the whole process; -debug exposes the Debug
	// events (http requests, flushes).
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	// Database file permissions (the config table holds the API key and the
	// password hash).
	if _, err := os.Stat(dbFile); os.IsNotExist(err) {
		f, err := os.OpenFile(dbFile, os.O_CREATE, 0o600)
		if err != nil {
			return fmt.Errorf("create db file: %w", err)
		}
		_ = f.Close()
	}

	sqlDB, err := db.Open(dbFile)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	if err := db.Migrate(sqlDB); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	st := store.NewStore(sqlDB)

	// Config store: seeds defaults on the very first run, then serves live
	// snapshots. This replaces the old config.toml entirely.
	ctx := context.Background()
	cfgStore, err := config.NewStore(ctx, sqlDB)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	warnIfExposed(cfgStore.Get(), *listen)

	// Startup sweeps.
	now := time.Now()
	if err := st.DeleteOrphanAttachments(ctx, now.Add(-24*time.Hour).UnixMilli()); err != nil {
		slog.Warn("sweep orphan attachments", "error", err)
	}
	if err := sweepEmptyChats(ctx, st, now.Add(-24*time.Hour).UnixMilli()); err != nil {
		slog.Warn("sweep empty chats", "error", err)
	}

	// Provider + MCP. The live provider starts unconfigured when the setup
	// hasn't provided an endpoint yet; Reconfigure (wired below) dials it the
	// moment base_url/api_key are set.
	boot := cfgStore.Get()
	prov := provider.NewLive(boot.Provider.BaseURL, boot.Provider.APIKey)
	hub := mcphub.New(cfgStore)
	hub.Connect(ctx)
	// Aggregated tool catalog: integrated (built-in, hardcoded) tools first,
	// then MCP tools; integrated tools win name collisions. The merged catalog
	// reads both sources live, so MCP servers added through config appear
	// without a restart. The outer layer applies the global per-tool defaults
	// from the config (settings UI) over the sources' own defaults.
	catalog := tools.WithDefaults(tools.Merge(tools.Builtin(st, cfgStore), hub), cfgStore)

	// Engine (server-scoped context: generations survive client disconnects).
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()
	eng := engine.New(serverCtx, st, prov, catalog, cfgStore, vision.New(cfgStore))
	if err := eng.RecoverCrashed(ctx); err != nil {
		slog.Warn("crash recovery", "error", err)
	}

	// Background title task: the ONLY writer of auto-generated titles. Runs
	// on the server context; its dedicated SSE hub fans title events out
	// independently of the engine's generation streams. It reads its provider
	// + task model live from the config store.
	titleSvc := titlegen.New(st, cfgStore)
	go titleSvc.Run(serverCtx)

	distFS, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		return fmt.Errorf("embedded frontend: %w", err)
	}
	a := auth.New(cfgStore)
	srv := api.New(cfgStore, st, a, eng, catalog, titleSvc.Hub(), distFS)

	// Live config wiring: re-dial the provider and reconcile MCP servers
	// whenever the relevant settings change. Both can take real time (MCP
	// dials), so they run async — Update() must not block on them. The
	// listen address is not live-editable: it is the -listen flag, fixed
	// for the process lifetime.
	cfgStore.Subscribe(func(c *config.Config) {
		prov.Reconfigure(c.Provider.BaseURL, c.Provider.APIKey)
		warnIfExposed(c, *listen)
		go func() {
			// When the reconciliation changed the tool catalog (a server was
			// added, removed, or reconnected), notify the connected clients
			// so they refetch /api/config — the settings save response
			// already went out before this finished, so without this the
			// tools menu stays stale until a page reload.
			if hub.Reload(serverCtx) {
				eng.PublishConfigChanged()
			}
		}()
	})

	// Bind the listener. The listen address is the -listen flag, fixed for
	// the process lifetime; a bind failure at startup is fatal.
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", *listen, err)
	}
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Deliberately NO WriteTimeout/ReadTimeout: they would kill SSE.
	}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("http listener exited", "addr", *listen, "error", err)
		}
	}()
	slog.Info("chattoneko serving", "addr", *listen, "db", dbFile, "setup_complete", cfgStore.Complete())

	// Graceful shutdown on SIGINT/SIGTERM.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	<-sigCtx.Done()

	slog.Info("shutting down")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	_ = httpSrv.Shutdown(shutdownCtx)
	eng.Shutdown() // cancels active generations and WAITS for their final persistence
	hub.Close()    // kills MCP stdio child processes
	_ = sqlDB.Close()
	serverCancel()
	return nil
}

func sweepEmptyChats(ctx context.Context, st *store.Store, cutoff int64) error {
	chats, err := st.ListEmptyChatsOlderThan(ctx, cutoff)
	if err != nil {
		return err
	}
	for _, c := range chats {
		if err := st.DeleteChat(ctx, c.ID); err != nil {
			return err
		}
	}
	return nil
}

// warnIfExposed flags the dangerous combination of auth disabled on a
// non-loopback address (the API is open to the whole network). The listen
// address is a CLI flag, not part of the config, so it is passed in.
func warnIfExposed(c *config.Config, addr string) {
	if !c.Auth.Enabled && !isLoopbackAddr(addr) {
		slog.Warn("auth is DISABLED and the listen address is not loopback-only — the API is fully open on the network")
	}
}

// isLoopbackAddr reports whether a "host:port" listen address binds only to
// the loopback interface. An empty host (":8080") means all interfaces in
// Go's net.Listen, so it is NOT loopback-only.
func isLoopbackAddr(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	host = strings.Trim(host, "[]")
	switch host {
	case "", "localhost", "127.0.0.1", "::1":
		return host != ""
	}
	return false
}
