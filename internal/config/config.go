// Package config stores the server configuration in SQLite — there is no
// config file. On first start an empty config table is seeded with hardcoded
// defaults; everything except auth is set through the API (initial setup,
// then admin changes) and takes effect live. Single-user auth is the one
// exception: it is driven entirely by the CHATTO_USERNAME / CHATTO_PASSWORD
// environment variables (see authFromEnv), read once at startup, and never
// stored in the database.
//
// Two tables back this package (migration 001_init.sql):
//
//   - config(key, value)   — global settings; structured values are JSON
//   - models(model_id, …)  — per-model metadata (modalities, context length,
//     reasoning-effort levels)
//
// Store is the live handle: Get() returns the current immutable snapshot,
// Update() applies a partial patch (transactional write, snapshot swap,
// subscriber notification) and Subscribe() lets components react to changes
// without restarts.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"

	_ "embed"

	"golang.org/x/net/http/httpguts"
)

// Hardcoded seed values, written to the config table on the very first run
// (empty table) and used as per-value fallbacks for empty/invalid rows.
const (
	// DefaultListen is the default of the -listen CLI flag (fixed at startup;
	// it is NOT stored in the config table).
	DefaultListen                      = ":8080"
	DefaultUploadMaxFileBytes          = 5 * 1024 * 1024 // 5 MiB
	DefaultMaxToolIterations           = 10
	DefaultMCPCallTimeoutSeconds       = 60
	DefaultContextLength         int64 = 131072 // 128K tokens
)

// Environment variables that drive single-user auth. There is no separate
// "login required" flag: login is required exactly when BOTH are set
// (non-empty after trimming). When either is missing, auth is disabled
// entirely. The password is used as plaintext. Env-driven auth is read once
// at startup; it is NOT editable through the API and NOT stored in the
// database.
const (
	EnvUsername = "CHATTO_USERNAME"
	EnvPassword = "CHATTO_PASSWORD"
)

// DefaultReasoningEfforts is the default set of selectable reasoning-effort
// levels; the default effort is the 2nd element.
var DefaultReasoningEfforts = []string{"low", "medium", "high"}

// defaultSystemPrompt is the seed system prompt.
//
//go:embed default_system.md
var defaultSystemPrompt string

// ProviderConfig holds the OpenAI-compatible provider settings. Requests
// always go to POST /chat/completions under BaseURL.
type ProviderConfig struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

// ModelsConfig holds the model whitelist and designated model ids.
type ModelsConfig struct {
	Whitelist        []string `json:"whitelist"`
	DefaultChatModel string   `json:"default_chat_model"`
	// DefaultTaskModel is the model id used by background tasks (title
	// generation). It talks to the same provider as chat.
	DefaultTaskModel string `json:"default_task_model"`
	// DefaultVisionModel is the model id used to describe images for chat
	// models that lack image input. Optional: when empty, images are sent
	// to the chat model as-is. It talks to the same provider as chat.
	DefaultVisionModel string `json:"default_vision_model"`
}

// MCPServerConfig declares one MCP server.
type MCPServerConfig struct {
	Name           string            `json:"name"`
	Transport      string            `json:"transport"` // stdio | http
	Command        string            `json:"command"`   // stdio
	Args           []string          `json:"args"`      // stdio
	URL            string            `json:"url"`       // http
	Headers        map[string]string `json:"headers"`   // http: extra request headers
	DefaultEnabled bool              `json:"default_enabled"`
}

// LimitsConfig holds resource limits.
type LimitsConfig struct {
	UploadMaxFileBytes int64 `json:"upload_max_file_bytes"`
	MaxToolIterations  int   `json:"max_tool_iterations"`
	// MCPCallTimeoutSeconds bounds a single MCP tool call (a hung MCP server
	// must not block the turn loop forever).
	MCPCallTimeoutSeconds int `json:"mcp_call_timeout_seconds"`
}

// AuthConfig holds single-user auth settings. It is populated from the
// CHATTO_USERNAME / CHATTO_PASSWORD environment variables at startup
// (authFromEnv) and never persisted to the database or exposed through the
// setup API. Password is the PLAINTEXT password from the environment.
type AuthConfig struct {
	Enabled  bool   `json:"enabled"`
	Username string `json:"username"`
	Password string `json:"-"` // never serialized to API responses
}

// authFromEnv derives the auth configuration from the environment. Login is
// required exactly when BOTH the username and password variables are set
// (non-empty after trimming); otherwise auth is disabled. The password is
// kept as plaintext.
func authFromEnv() AuthConfig {
	user := strings.TrimSpace(os.Getenv(EnvUsername))
	pass := strings.TrimSpace(os.Getenv(EnvPassword))
	if user == "" || pass == "" {
		return AuthConfig{} // login disabled
	}
	return AuthConfig{Enabled: true, Username: user, Password: pass}
}

// Config is one immutable configuration snapshot. Read it via Store.Get();
// never mutate a shared snapshot in place.
type Config struct {
	SystemPrompt string            `json:"system_prompt"`
	Provider     ProviderConfig    `json:"provider"`
	Models       ModelsConfig      `json:"models"`
	MCPServers   []MCPServerConfig `json:"mcp_servers"`
	Limits       LimitsConfig      `json:"limits"`
	Auth         AuthConfig        `json:"auth"`
	// ToolDefaults is the global per-tool default toggle (settings UI):
	// tool display name → enabled. It overrides the catalog's own default
	// (integrated tools' hardcoded DefaultEnabled, MCP tools' server
	// default_enabled) for chats that carry no override of their own. A
	// tool absent from the map keeps the catalog default.
	ToolDefaults map[string]bool `json:"tool_defaults"`
}

// clone returns a deep copy so patches never mutate a published snapshot.
func (c *Config) clone() *Config {
	cp := *c
	cp.Models.Whitelist = append([]string(nil), c.Models.Whitelist...)
	cp.MCPServers = make([]MCPServerConfig, len(c.MCPServers))
	for i, s := range c.MCPServers {
		s.Args = append([]string(nil), s.Args...)
		if s.Headers != nil {
			s.Headers = make(map[string]string, len(s.Headers))
			for k, v := range c.MCPServers[i].Headers {
				s.Headers[k] = v
			}
		}
		cp.MCPServers[i] = s
	}
	if c.ToolDefaults != nil {
		cp.ToolDefaults = make(map[string]bool, len(c.ToolDefaults))
		for k, v := range c.ToolDefaults {
			cp.ToolDefaults[k] = v
		}
	}
	return &cp
}

// Complete reports whether the minimum settings needed to run chats are
// present (provider endpoint/key + both designated models). An incomplete
// config means "setup still needed" — the server still runs and serves the
// API/UI.
func (c *Config) Complete() bool {
	return c.Provider.BaseURL != "" && c.Provider.APIKey != "" &&
		c.Models.DefaultChatModel != "" && c.Models.DefaultTaskModel != ""
}

// finalize fills every empty/zero/invalid value with its fallback and drops
// structurally broken entries. It runs on load AND before every update is
// persisted, so both paths end in the same sanitized shape.
func (c *Config) finalize() {
	if strings.TrimSpace(c.SystemPrompt) == "" {
		c.SystemPrompt = strings.TrimSpace(defaultSystemPrompt)
	}
	if c.Limits.UploadMaxFileBytes <= 0 {
		c.Limits.UploadMaxFileBytes = DefaultUploadMaxFileBytes
	}
	if c.Limits.MaxToolIterations <= 0 {
		c.Limits.MaxToolIterations = DefaultMaxToolIterations
	}
	if c.Limits.MCPCallTimeoutSeconds <= 0 {
		c.Limits.MCPCallTimeoutSeconds = DefaultMCPCallTimeoutSeconds
	}
	c.sanitizeWhitelist()
	c.sanitizeMCPServers()
}

// sanitizeWhitelist drops empty and duplicate model ids and clears a
// designated model (default chat/task/vision) that is not whitelisted. Designated
// models must be members of the whitelist (the settings UI flags them from
// whitelisted cards), so auto-adding them would paper over stale ids.
func (c *Config) sanitizeWhitelist() {
	c.Models.Whitelist = filterEmpty(c.Models.Whitelist)
	seen := make(map[string]bool, len(c.Models.Whitelist))
	for _, m := range c.Models.Whitelist {
		seen[m] = true
	}
	if m := strings.TrimSpace(c.Models.DefaultChatModel); !seen[m] {
		c.Models.DefaultChatModel = ""
	}
	if m := strings.TrimSpace(c.Models.DefaultTaskModel); !seen[m] {
		c.Models.DefaultTaskModel = ""
	}
	if m := strings.TrimSpace(c.Models.DefaultVisionModel); !seen[m] {
		c.Models.DefaultVisionModel = ""
	}
}

// sanitizeMCPServers drops MCP server entries that cannot work — a broken
// entry must not keep the server from serving.
func (c *Config) sanitizeMCPServers() {
	seen := map[string]bool{}
	clean := make([]MCPServerConfig, 0, len(c.MCPServers))
	for _, s := range c.MCPServers {
		s.Name = strings.TrimSpace(s.Name)
		s.Command = strings.TrimSpace(s.Command)
		s.URL = strings.TrimSpace(s.URL)
		switch {
		case s.Name == "":
			slog.Warn("mcp_servers: dropping entry without a name")
			continue
		case seen[s.Name]:
			slog.Warn("mcp_servers: dropping duplicate entry", "name", s.Name)
			continue
		case s.Transport != "stdio" && s.Transport != "http":
			slog.Warn("mcp_servers: dropping entry with invalid transport", "name", s.Name, "transport", s.Transport)
			continue
		case s.Transport == "stdio" && s.Command == "":
			slog.Warn("mcp_servers: dropping stdio entry without command", "name", s.Name)
			continue
		case s.Transport == "http" && s.URL == "":
			slog.Warn("mcp_servers: dropping http entry without url", "name", s.Name)
			continue
		}
		// Copy the entry's slices/maps so the published snapshot never
		// aliases the patch they came from.
		s.Args = append([]string(nil), s.Args...)
		if s.Transport == "stdio" && len(s.Headers) > 0 {
			slog.Warn("mcp_servers: ignoring headers on stdio entry", "name", s.Name)
			s.Headers = nil
		}
		if len(s.Headers) > 0 {
			// Header names/values go straight into HTTP requests: trim them
			// and drop empty values and invalid header names (a broken name
			// would fail the MCP call at request time with a confusing
			// error). Header values round-trip through the setup API, so an
			// empty value clears the header.
			cleaned := make(map[string]string, len(s.Headers))
			for k, v := range s.Headers {
				k = strings.TrimSpace(k)
				v = strings.TrimSpace(v)
				if k == "" || v == "" || !httpguts.ValidHeaderFieldName(k) {
					continue
				}
				cleaned[k] = v
			}
			s.Headers = cleaned
		}
		seen[s.Name] = true
		clean = append(clean, s)
	}
	c.MCPServers = clean
}

// validate enforces the invariants that no fallback can fix. Called before
// an update is persisted (never at load time: stored config must not keep
// the server from starting — broken values fall back via finalize).
func (c *Config) validate() error {
	if c.Auth.Enabled {
		if strings.TrimSpace(c.Auth.Username) == "" {
			return fmt.Errorf("auth.username is required when auth is enabled")
		}
		if strings.TrimSpace(c.Auth.Password) == "" {
			return fmt.Errorf("a password must be set before auth can be enabled")
		}
	}
	return nil
}

// MCPServerEqual reports whether two MCP server configs are identical
// (used by the MCP hub to decide if a server needs reconnecting). Both sides
// are normalized by sanitizeMCPServers before comparison, so nil-vs-empty
// slices/maps are consistent.
func MCPServerEqual(a, b MCPServerConfig) bool {
	return reflect.DeepEqual(a, b)
}
