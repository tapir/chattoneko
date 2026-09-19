// Chattoneko — a self-hosted ChatGPT-style chat app in one Go binary:
// embedded Svelte SPA, REST/SSE API, SQLite storage, one OpenAI-compatible
// provider, built-in and MCP tools.
//
// There is no config file. Settings live in the SQLite config and models
// tables and apply live (see the subscription in run()). Two exceptions: the
// listen address is the -listen flag (default :8080), fixed for the process
// lifetime, and single-user auth comes from CHATTO_USERNAME /
// CHATTO_PASSWORD — login is required exactly when both are set, the password
// is used as plaintext, and neither value reaches the database. An empty
// config table is seeded with defaults, and the server reports setup mode
// until the provider and model fields are set through the API.
//
// In a container the process starts as root, makes the data directory writable
// by uid 1000 and drops to it before opening anything (droproot_linux.go), so
// a bind mount needs no host-side chown and nothing untrusted — ffmpeg parses
// uploaded media in-process — ever runs as root.
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
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"chattoneko/internal/api"
	"chattoneko/internal/auth"
	"chattoneko/internal/config"
	"chattoneko/internal/db"
	"chattoneko/internal/engine"
	"chattoneko/internal/mcphub"
	"chattoneko/internal/media"
	"chattoneko/internal/provider"
	"chattoneko/internal/store"
	"chattoneko/internal/titlegen"
	"chattoneko/internal/tools"
)

//go:embed web/dist
var webFS embed.FS

// version is stamped at link time (-ldflags -X main.version=…) and served on
// /api/meta; hand-built binaries keep this default.
var version = "1.0.0-local"

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

	// Default to chatto.db beside the binary so the working directory does not
	// decide which database is opened.
	dbFile := *dbPath
	if dbFile == "" {
		dbFile = "chatto.db" // fallback if the executable path cannot be resolved
		if exe, err := os.Executable(); err == nil {
			dbFile = filepath.Join(filepath.Dir(exe), "chatto.db")
		}
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	// ffmpeg is a hard dependency, not a runtime surprise: without it every
	// picture and recording upload fails as ErrUnsupported, which the API
	// reports as a 415 blaming the user's file.
	if _, err := exec.LookPath(media.Binary()); err != nil {
		return fmt.Errorf("ffmpeg not found (%q): install it or point %s at it", media.Binary(), media.EnvBinary)
	}

	// Before anything is opened: the container starts as root only so a
	// bind-mounted data directory can be made writable, then the process drops
	// to 1000:1000 for the rest of its life (see droproot_linux.go).
	if err := dropRoot(filepath.Dir(dbFile)); err != nil {
		return fmt.Errorf("drop privileges: %w", err)
	}

	// 0o600: the config table holds the provider API key.
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

	ctx := context.Background()
	cfgStore, err := config.NewStore(ctx, sqlDB)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	warnIfExposed(cfgStore.Get(), *listen)

	now := time.Now()
	if err := media.Sweep(); err != nil {
		slog.Warn("sweep conversion temp files", "error", err)
	}
	if err := st.DeleteOrphanAttachments(ctx, now.Add(-24*time.Hour).UnixMilli()); err != nil {
		slog.Warn("sweep orphan attachments", "error", err)
	}
	if err := sweepEmptyChats(ctx, st, now.Add(-24*time.Hour).UnixMilli()); err != nil {
		slog.Warn("sweep empty chats", "error", err)
	}

	boot := cfgStore.Get()
	prov := provider.NewLive(boot.Provider.BaseURL, boot.Provider.APIKey)
	hub := mcphub.New(cfgStore)
	hub.Reload(ctx)
	// Built-ins precede MCP servers: Merge gives the first source priority on
	// name collisions.
	catalog := tools.Merge(cfgStore, tools.Builtin(st, cfgStore), hub)

	// Server-scoped context: generations outlive the request that started them.
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()
	eng := engine.New(serverCtx, st, prov, catalog, cfgStore)
	if err := eng.RecoverCrashed(ctx); err != nil {
		slog.Warn("crash recovery", "error", err)
	}

	titleSvc := titlegen.New(st, cfgStore, eng.PublishTitle)
	go titleSvc.Run(serverCtx)

	distFS, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		return fmt.Errorf("embedded frontend: %w", err)
	}
	a := auth.New(cfgStore)
	srv := api.New(cfgStore, st, a, eng, catalog, distFS, version)

	// Config saves re-dial the provider and reconcile MCP servers. hub.Reload
	// dials every server, so it runs off the subscriber: config.Update must
	// not block on it.
	cfgStore.Subscribe(func(c *config.Config) {
		prov.Reconfigure(c.Provider.BaseURL, c.Provider.APIKey)
		warnIfExposed(c, *listen)
		go func() {
			// Reload before publishing: config_changed makes clients refetch
			// /api/config, which must already list the new tool catalog.
			hub.Reload(serverCtx)
			eng.PublishConfigChanged()
		}()
	})

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

	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	<-sigCtx.Done()

	slog.Info("shutting down")
	// Shutdown returns only once every connection is idle, and an open tab's
	// SSE stream never is — so it gets a short drain window and Close below
	// drops what is left. Letting it run its full course would spend the whole
	// `docker stop` grace period here and SIGKILL the teardown that follows.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelShutdown()
	_ = httpSrv.Shutdown(shutdownCtx)
	eng.Shutdown() // waits for in-flight generations to persist before the DB closes
	// Cancel the server context BEFORE hub.Close: a config save can have an
	// async hub.Reload still dialing (the HTTP shutdown above doesn't stop
	// it), and an alive context keeps those dials running up to their 30s
	// timeout — Close would block on them via reloadMu. Canceled first, the
	// dials abort at once and Close reaps everything they managed to open.
	serverCancel()
	_ = httpSrv.Close()
	hub.Close()
	_ = sqlDB.Close()
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

// warnIfExposed logs when auth is disabled on a non-loopback address, which
// leaves the API open to the network. addr is the -listen flag, not config.
func warnIfExposed(c *config.Config, addr string) {
	if !c.Auth.Enabled && !isLoopbackAddr(addr) {
		slog.Warn("auth is DISABLED and the listen address is not loopback-only — the API is fully open on the network")
	}
}

// isLoopbackAddr reports whether a "host:port" listen address binds loopback
// only. An empty host (":8080") binds every interface, so it is not
// loopback-only.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
