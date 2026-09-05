// Package api serves the REST API, SSE streams, and the embedded SPA.
package api

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"chattoneko/internal/auth"
	"chattoneko/internal/config"
	"chattoneko/internal/engine"
	"chattoneko/internal/mcphub"
	"chattoneko/internal/store"
	"chattoneko/internal/titlegen"
)

// ToolCatalog is the subset of the aggregated tool catalog the API needs
// (the tool listing for /api/config). The merged integrated+MCP catalog
// (internal/tools.Merged) satisfies it.
type ToolCatalog interface {
	Tools() []mcphub.Entry
}

// Server wires the HTTP surface.
type Server struct {
	cfg    *config.Store
	store  *store.Store
	auth   *auth.Auth
	engine *engine.Engine
	tools  ToolCatalog
	// titles is the title task's independent event fan-out (SSE below).
	titles *titlegen.Hub

	staticFS fs.FS
}

// New builds the server. staticFS is the embedded web/dist tree.
func New(cfg *config.Store, st *store.Store, a *auth.Auth, eng *engine.Engine, tools ToolCatalog, titles *titlegen.Hub, staticFS fs.FS) *Server {
	return &Server{cfg: cfg, store: st, auth: a, engine: eng, tools: tools, titles: titles, staticFS: staticFS}
}

// Handler returns the root mux. http.Server MUST NOT set WriteTimeout
// (it would kill SSE); only ReadHeaderTimeout belongs on the server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Auth surface (login/meta public; the rest gated by middleware below).
	mux.HandleFunc("GET /api/meta", s.handleMeta)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)

	gated := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.auth.Middleware(http.HandlerFunc(h)))
	}
	gated("GET /api/auth/me", s.handleMe)
	gated("GET /api/config", s.handleGetConfig)
	gated("GET /api/setup", s.handleGetSetup)
	gated("PUT /api/setup", s.handlePutSetup)
	gated("POST /api/setup/models", s.handleSetupModels)
	gated("GET /api/chats", s.handleListChats)
	gated("GET /api/stream", s.handleGlobalStream)
	gated("GET /api/stream/titles", s.handleTitleStream)
	gated("POST /api/chats", s.handleCreateChat)
	gated("GET /api/chats/{id}", s.handleGetChat)
	gated("GET /api/chats/{id}/log", s.handleChatLog)
	gated("PATCH /api/chats/{id}", s.handlePatchChat)
	gated("DELETE /api/chats/{id}", s.handleDeleteChat)
	gated("POST /api/chats/{id}/messages", s.handleSendMessage)
	gated("PATCH /api/chats/{id}/messages/{mid}", s.handleEditMessage)
	gated("POST /api/chats/{id}/regenerate", s.handleRegenerate)
	gated("DELETE /api/chats/{id}/generation", s.handleStopGeneration)
	gated("GET /api/chats/{id}/stream", s.handleStream)
	gated("POST /api/chats/{id}/attachments", s.handleUpload)
	gated("GET /api/attachments/{id}", s.handleGetAttachment)
	gated("GET /api/attachments/{id}/description", s.handleGetAttachmentDescription)

	// Static SPA with fallback for client-side routes.
	mux.Handle("/", spaHandler{static: http.FileServerFS(s.staticFS), fs: s.staticFS})

	return logRequests(corsMiddleware(mux))
}

// spaHandler serves embedded files, falling back to index.html for
// non-/api paths (client-side routing).
type spaHandler struct {
	static http.Handler
	fs     fs.FS
}

func (h spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	// Hashed build assets are immutable; HTML (index + SPA fallback) must
	// never be cached or clients can be stuck on a stale bundle.
	if strings.HasPrefix(p, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		h.static.ServeHTTP(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	if p != "" {
		if _, err := fs.Stat(h.fs, p); err != nil {
			r2 := *r
			r2.URL.Path = "/"
			h.static.ServeHTTP(w, &r2)
			return
		}
	}
	h.static.ServeHTTP(w, r)
}

// statusRecorder captures the response status so the logging middleware
// can surface client/server errors. Flush is forwarded explicitly: the SSE
// handler relies on http.Flusher, which a bare wrapper would hide.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(code int) {
	// Record only the first code: net/http rejects duplicate WriteHeader
	// calls, so logging the later (discarded) one would misreport status.
	if !r.written {
		r.written = true
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the wrapped writer so http.ResponseController (and any
// future wrapper-aware code) sees the real ResponseWriter's optional
// interfaces (Flusher, Hijacker, ...) instead of stopping at this recorder.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		args := []any{"method", r.Method, "path", r.URL.Path, "status", rec.status, "dur", time.Since(start)}
		switch {
		case rec.status >= 500:
			// Server errors are real problems: always visible.
			slog.Error("http", args...)
		case rec.status >= 400:
			// Benign 4xx (SPA probing, stale clients, auth misses) are routine
			// client behavior, not warnings — keep them out of the warn stream.
			slog.Info("http", args...)
		case !isSSEPath(r.URL.Path):
			slog.Debug("http", args...)
		}
	})
}

// isSSEPath reports whether the path is one of the long-lived SSE
// endpoints (chat stream, global stream, title stream): the connections
// stay open for hours, so a per-request log line for them is noise.
func isSSEPath(path string) bool {
	return path == "/api/stream" ||
		strings.HasPrefix(path, "/api/stream/") ||
		strings.HasSuffix(path, "/stream")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	// Every JSON API response reflects state that can change at any time
	// (config is live-editable through the setup API). Without an explicit
	// opt-out, browser/proxy HTTP caching can serve a stale whitelist long
	// after the config changed.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// internalError logs the real error and returns a generic 500: raw
// store/DB errors are an internal detail, not part of the API contract.
func internalError(w http.ResponseWriter, op string, err error) {
	slog.Error("api: "+op, "error", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// maxJSONBodyBytes caps JSON request bodies (the multipart upload route has
// its own ceiling). Generous for message payloads, tight enough to stop
// pathological bodies.
const maxJSONBodyBytes = 1 << 20 // 1 MiB

// decodeJSON decodes a JSON request body with the size cap applied.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	return json.NewDecoder(r.Body).Decode(v)
}

// writeBodyError answers a request-body decode failure: an over-cap body
// (MaxBytesReader) becomes 413, anything else a 400.
func writeBodyError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	writeError(w, http.StatusBadRequest, "invalid JSON body")
}
