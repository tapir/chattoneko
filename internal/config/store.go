package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// config-table keys.
const (
	keySystemPrompt          = "system_prompt"
	keyProviderBaseURL       = "provider_base_url"
	keyProviderAPIKey        = "provider_api_key"
	keyModelWhitelist        = "model_whitelist"
	keyDefaultChatModel      = "default_chat_model"
	keyDefaultTaskModel      = "default_task_model"
	keyDefaultVisionModel    = "default_vision_model"
	keyMCPServers            = "mcp_servers"
	keyUploadMaxFileBytes    = "upload_max_file_bytes"
	keyMaxToolIterations     = "max_tool_iterations"
	keyMCPCallTimeoutSeconds = "mcp_call_timeout_seconds"
	keyToolDefaults          = "tool_defaults"
	// Auth has NO keys here: it is env-var driven (authFromEnv), read once
	// at startup, and never persisted to the config table.
)

// Store is the live configuration handle. It owns the current immutable
// snapshot (swapped atomically on every write) and a subscriber list that is
// notified after each successful update. All writes are serialized through
// updateMu and applied in a single transaction: a snapshot is only ever
// swapped in after its row set is committed.
type Store struct {
	db *sql.DB

	updateMu sync.Mutex // serializes Update/forceSet

	snap atomic.Pointer[Config]

	subMu sync.Mutex
	subs  []func(*Config)
}

// NewStore opens the config store over db (migrations already applied). When
// the config table is completely empty it is seeded with the default config
// (first run).
func NewStore(ctx context.Context, db *sql.DB) (*Store, error) {
	if err := seedIfEmpty(ctx, db); err != nil {
		return nil, fmt.Errorf("seed defaults: %w", err)
	}
	s := &Store{db: db}
	if err := s.reload(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// seedIfEmpty writes the default config when the config table has no rows at
// all. Everything not seeded here reads as empty/null until set through the
// API.
func seedIfEmpty(ctx context.Context, db *sql.DB) error {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM config`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	slog.Info("config table empty; seeding defaults")
	// Auth is env-var driven and never seeded/persisted.
	seed := map[string]string{
		keySystemPrompt:          strings.TrimSpace(defaultSystemPrompt),
		keyUploadMaxFileBytes:    strconv.FormatInt(DefaultUploadMaxFileBytes, 10),
		keyMaxToolIterations:     strconv.Itoa(DefaultMaxToolIterations),
		keyMCPCallTimeoutSeconds: strconv.Itoa(DefaultMCPCallTimeoutSeconds),
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for k, v := range seed {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO config (key, value, updated_at) VALUES (?, ?, ?)`,
			k, v, now); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// Get returns the current snapshot. Never nil after NewStore.
func (s *Store) Get() *Config { return s.snap.Load() }

// persist writes the config rows, optionally prunes the models table and
// upserts model metadata — all in ONE transaction — then swaps the
// snapshot. The snapshot is only ever swapped after everything is committed,
// so a failure mid-way leaves the old snapshot fully in effect.
func (s *Store) persist(ctx context.Context, c *Config, pruneKeep *[]string, metas []ModelMeta) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	if err := writeConfigRows(ctx, tx, c, now); err != nil {
		_ = tx.Rollback()
		return err
	}
	if pruneKeep != nil {
		if err := pruneModelMetas(ctx, tx, *pruneKeep); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if len(metas) > 0 {
		if err := upsertModelMetas(ctx, tx, metas, now); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.snap.Store(c)
	return nil
}

// writeConfigRows persists the full snapshot as the complete config row set.
func writeConfigRows(ctx context.Context, tx *sql.Tx, c *Config, now int64) error {
	whitelist, _ := json.Marshal(c.Models.Whitelist)
	servers, _ := json.Marshal(c.MCPServers)
	toolDefaults, _ := json.Marshal(c.ToolDefaults)
	rows := map[string]string{
		keySystemPrompt:          c.SystemPrompt,
		keyProviderBaseURL:       c.Provider.BaseURL,
		keyProviderAPIKey:        c.Provider.APIKey,
		keyModelWhitelist:        string(whitelist),
		keyDefaultChatModel:      c.Models.DefaultChatModel,
		keyDefaultTaskModel:      c.Models.DefaultTaskModel,
		keyDefaultVisionModel:    c.Models.DefaultVisionModel,
		keyMCPServers:            string(servers),
		keyUploadMaxFileBytes:    strconv.FormatInt(c.Limits.UploadMaxFileBytes, 10),
		keyMaxToolIterations:     strconv.Itoa(c.Limits.MaxToolIterations),
		keyMCPCallTimeoutSeconds: strconv.Itoa(c.Limits.MCPCallTimeoutSeconds),
		keyToolDefaults:          string(toolDefaults),
		// Auth is env-var driven and never persisted.
	}
	// One transaction, so write order is unobservable — range the map.
	for k, v := range rows {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO config (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			k, v, now); err != nil {
			return err
		}
	}
	return nil
}

// Complete reports whether the minimum chat settings are present.
func (s *Store) Complete() bool { return s.Get().Complete() }

// Subscribe registers fn to be called (synchronously, in registration order)
// after every successful config write, with the new snapshot. Registration
// itself does NOT fire fn.
func (s *Store) Subscribe(fn func(*Config)) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	s.subs = append(s.subs, fn)
}

func (s *Store) notify(c *Config) {
	s.subMu.Lock()
	subs := make([]func(*Config), len(s.subs))
	copy(subs, s.subs)
	s.subMu.Unlock()
	for _, fn := range subs {
		fn(c)
	}
}

// reload reads every config row and swaps in a finalized snapshot.
func (s *Store) reload(ctx context.Context) error {
	c, err := loadSnapshot(ctx, s.db)
	if err != nil {
		return err
	}
	s.snap.Store(c)
	return nil
}

// loadSnapshot reads the config table into a finalized snapshot. Missing or
// empty keys fall back to their defaults via finalize; corrupt structured
// values are dropped with a warning (finalize then substitutes defaults).
func loadSnapshot(ctx context.Context, db *sql.DB) (*Config, error) {
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM config`)
	if err != nil {
		return nil, fmt.Errorf("query config: %w", err)
	}
	defer rows.Close()
	kv := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan config row: %w", err)
		}
		kv[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	c := &Config{}
	c.SystemPrompt = kv[keySystemPrompt]
	c.Provider.BaseURL = kv[keyProviderBaseURL]
	c.Provider.APIKey = kv[keyProviderAPIKey]
	c.Models.DefaultChatModel = kv[keyDefaultChatModel]
	c.Models.DefaultTaskModel = kv[keyDefaultTaskModel]
	c.Models.DefaultVisionModel = kv[keyDefaultVisionModel]
	// Auth never comes from the database: it is derived from the
	// CHATTO_USERNAME / CHATTO_PASSWORD environment variables.
	c.Auth = authFromEnv()

	if v := kv[keyModelWhitelist]; v != "" {
		if err := json.Unmarshal([]byte(v), &c.Models.Whitelist); err != nil {
			slog.Warn("config: corrupt model_whitelist, ignoring", "error", err)
		}
	}
	if v := kv[keyMCPServers]; v != "" {
		if err := json.Unmarshal([]byte(v), &c.MCPServers); err != nil {
			slog.Warn("config: corrupt mcp_servers, ignoring", "error", err)
		}
	}
	if v := kv[keyToolDefaults]; v != "" {
		if err := json.Unmarshal([]byte(v), &c.ToolDefaults); err != nil {
			slog.Warn("config: corrupt tool_defaults, ignoring", "error", err)
		}
	}
	if v := kv[keyUploadMaxFileBytes]; v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.Limits.UploadMaxFileBytes = n
		} else {
			slog.Warn("config: corrupt upload_max_file_bytes, ignoring", "value", v)
		}
	}
	if v := kv[keyMaxToolIterations]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Limits.MaxToolIterations = n
		} else {
			slog.Warn("config: corrupt max_tool_iterations, ignoring", "value", v)
		}
	}
	if v := kv[keyMCPCallTimeoutSeconds]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Limits.MCPCallTimeoutSeconds = n
		} else {
			slog.Warn("config: corrupt mcp_call_timeout_seconds, ignoring", "value", v)
		}
	}

	c.finalize()
	return c, nil
}

// forceSet replaces the ENTIRE config with c (finalized, then validated and
// persisted). Test-only: API writes go through Update with a Patch.
func (s *Store) forceSet(ctx context.Context, c Config) (*Config, error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	next := c.clone()
	next.finalize()
	if err := next.validate(); err != nil {
		return nil, err
	}
	if err := s.persist(ctx, next, nil, nil); err != nil {
		return nil, err
	}
	s.notify(next)
	return next, nil
}
