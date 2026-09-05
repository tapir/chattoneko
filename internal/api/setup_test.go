package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"chattoneko/internal/config"
)

// ---- GET /api/setup ----

// GET /api/setup returns secrets verbatim: the settings UI displays and
// edits the provider API key and MCP header values in place.
func TestGetSetupExposesSecrets(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)

	// Seed a provider key and an MCP server with a header through the store
	// so GET must return both verbatim.
	if _, err := ts.cfg.Update(t.Context(), patchFromMap(t, map[string]any{
		"provider": map[string]any{"base_url": "https://p.example", "api_key": "sk-super-secret"},
		"mcp_servers": []map[string]any{
			{"name": "srv", "transport": "http", "url": "https://mcp.example",
				"headers": map[string]any{"Authorization": "Bearer mcp-secret"}},
		},
	})); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := ts.do(t, "GET", "/api/setup", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "sk-super-secret") {
		t.Fatalf("api_key missing from response: %s", body)
	}
	if !strings.Contains(body, "Bearer mcp-secret") {
		t.Fatalf("mcp header value missing from response: %s", body)
	}
	// Auth is env-var driven and must not appear in the setup payload at all.
	if strings.Contains(body, "\"auth\"") {
		t.Fatalf("auth leaked into setup response: %s", body)
	}

	var out struct {
		Complete bool `json:"complete"`
		Config   struct {
			Provider struct {
				BaseURL   string `json:"base_url"`
				APIKey    string `json:"api_key"`
				APIKeySet bool   `json:"api_key_set"`
			} `json:"provider"`
			MCPServers []struct {
				Name    string            `json:"name"`
				Headers map[string]string `json:"headers"`
			} `json:"mcp_servers"`
		} `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Config.Provider.BaseURL != "https://p.example" {
		t.Errorf("base_url = %q", out.Config.Provider.BaseURL)
	}
	if out.Config.Provider.APIKey != "sk-super-secret" || !out.Config.Provider.APIKeySet {
		t.Errorf("api_key = %q, api_key_set = %v", out.Config.Provider.APIKey, out.Config.Provider.APIKeySet)
	}
	if len(out.Config.MCPServers) != 1 || out.Config.MCPServers[0].Headers["Authorization"] != "Bearer mcp-secret" {
		t.Errorf("mcp_servers = %+v", out.Config.MCPServers)
	}
	// default_task_model is still empty → not complete.
	if out.Complete {
		t.Error("config should not be complete yet (no task model)")
	}
}

func TestMetaExposesSetupComplete(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)

	var meta struct {
		AuthEnabled   bool `json:"auth_enabled"`
		SetupComplete bool `json:"setup_complete"`
	}
	rec := ts.do(t, "GET", "/api/meta", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("meta status = %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.SetupComplete {
		t.Fatal("fresh server should report setup_complete=false")
	}

	// Complete the provider + models config.
	if _, err := ts.cfg.Update(t.Context(), patchFromMap(t, map[string]any{
		"provider": map[string]any{"base_url": "https://p.example", "api_key": "sk-x"},
		// The designated task model must be whitelisted (designated models
		// not on the whitelist are cleared by sanitizeWhitelist).
		"models": map[string]any{"whitelist": []string{"m", "task"}, "default_task_model": "task"},
	})); err != nil {
		t.Fatalf("update: %v", err)
	}
	rec = ts.do(t, "GET", "/api/meta", nil, nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if !meta.SetupComplete {
		t.Fatal("after provider+models set, setup_complete should be true")
	}
}

// ---- PUT /api/setup ----

func TestPutSetupPartialUpdate(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)

	// Update only the system prompt.
	rec := ts.do(t, "PUT", "/api/setup", map[string]any{"system_prompt": "custom"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if got := ts.cfg.Get().SystemPrompt; got != "custom" {
		t.Fatalf("system_prompt = %q, want custom", got)
	}
	// Whitelist (seeded by the harness) untouched by the partial update.
	if wl := ts.cfg.Get().Models.Whitelist; len(wl) == 0 {
		t.Fatal("whitelist was wiped by a partial update")
	}
}

// Auth is env-var driven: a PUT /api/setup carrying an "auth" object must
// be accepted but leave the stored config's auth untouched (field ignored).
func TestPutSetupIgnoresAuth(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, true) // auth enabled, password "pw"
	rec := ts.do(t, "POST", "/api/auth/login", map[string]string{"username": "admin", "password": "pw"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	var login struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)

	rec = ts.do(t, "PUT", "/api/setup", map[string]any{
		"auth": map[string]any{"enabled": false, "username": "x", "password": "y"},
	}, map[string]string{"Authorization": "Bearer " + login.Token})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT with ignored auth field: status = %d, body %s", rec.Code, rec.Body)
	}
	a := ts.cfg.Get().Auth
	if !a.Enabled || a.Username != "admin" || a.Password != "pw" {
		t.Fatalf("auth must be unchanged by the patch, got %+v", a)
	}
}

func TestPutSetupAuthGate(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, true) // auth enabled, password "pw"

	// Unauthenticated setup access is blocked.
	rec := ts.do(t, "GET", "/api/setup", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/setup: status = %d", rec.Code)
	}

	// Log in, then setup works with the bearer token.
	rec = ts.do(t, "POST", "/api/auth/login", map[string]string{"username": "admin", "password": "pw"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	var login struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)

	rec = ts.do(t, "GET", "/api/setup", nil, map[string]string{"Authorization": "Bearer " + login.Token})
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated GET /api/setup: status = %d", rec.Code)
	}
	rec = ts.do(t, "PUT", "/api/setup", map[string]any{"system_prompt": "via auth"},
		map[string]string{"Authorization": "Bearer " + login.Token})
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated PUT /api/setup: status = %d %s", rec.Code, rec.Body)
	}
	if got := ts.cfg.Get().SystemPrompt; got != "via auth" {
		t.Fatalf("system_prompt = %q after authed PUT", got)
	}
}

// ---- POST /api/setup/models ----

// modelsSrv serves an OpenRouter-style /models endpoint.
func modelsSrv(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[
			{"id":"or/vision","context_length":200000,
			 "architecture":{"input_modalities":["text","image"],"output_modalities":["text"]},
			 "reasoning":{"supported_efforts":["low","medium"],"default_effort":"low"}},
			{"id":"or/plain","object":"model"}
		]}`)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestSetupModelsFetchAndDefaults(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)
	ms := modelsSrv(t)

	// Point the provider at the fake /models endpoint.
	if _, err := ts.cfg.Update(t.Context(), patchFromMap(t, map[string]any{
		"provider": map[string]any{"base_url": ms.URL, "api_key": "sk-x"},
	})); err != nil {
		t.Fatalf("set provider: %v", err)
	}

	rec := ts.do(t, "POST", "/api/setup/models", map[string]any{
		"model_ids": []string{"or/vision", "or/plain", "or/unknown"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var out struct {
		Models []struct {
			ID               string   `json:"id"`
			ContextWindow    int64    `json:"context_window"`
			InputModality    []string `json:"input_modality"`
			OutputModality   []string `json:"output_modality"`
			ReasoningEfforts []string `json:"reasoning_efforts"`
			ReasoningDefault string   `json:"reasoning_default"`
		} `json:"models"`
		Source   map[string]string `json:"source"`
		Provider string            `json:"provider"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Provider != "ok" {
		t.Fatalf("provider = %q", out.Provider)
	}
	if len(out.Models) != 3 {
		t.Fatalf("models = %+v", out.Models)
	}

	byID := map[string]int{}
	for i, m := range out.Models {
		byID[m.ID] = i
	}
	// or/vision: fully provider-reported.
	v := out.Models[byID["or/vision"]]
	if v.ContextWindow != 200000 || out.Source["or/vision"] != "provider" {
		t.Errorf("or/vision = %+v (source %q)", v, out.Source["or/vision"])
	}
	if !slices.Equal(v.InputModality, []string{"text", "image"}) {
		t.Errorf("or/vision input = %v", v.InputModality)
	}
	if !slices.Equal(v.ReasoningEfforts, []string{"low", "medium"}) || v.ReasoningDefault != "low" {
		t.Errorf("or/vision reasoning = %v / %q", v.ReasoningEfforts, v.ReasoningDefault)
	}
	// or/plain: present in the list but no fields → all defaults, source provider.
	p := out.Models[byID["or/plain"]]
	if p.ContextWindow != 131072 || out.Source["or/plain"] != "provider" {
		t.Errorf("or/plain = %+v (source %q)", p, out.Source["or/plain"])
	}
	// or/unknown: not in the list → all defaults, source defaults.
	u := out.Models[byID["or/unknown"]]
	if u.ContextWindow != 131072 || out.Source["or/unknown"] != "defaults" {
		t.Errorf("or/unknown = %+v (source %q)", u, out.Source["or/unknown"])
	}
	if !slices.Equal(u.ReasoningEfforts, []string{"low", "medium", "high"}) || u.ReasoningDefault != "medium" {
		t.Errorf("or/unknown reasoning = %v / %q", u.ReasoningEfforts, u.ReasoningDefault)
	}
	if !slices.Equal(u.InputModality, []string{"text"}) || !slices.Equal(u.OutputModality, []string{"text"}) {
		t.Errorf("or/unknown modalities = %v / %v", u.InputModality, u.OutputModality)
	}

	// The metadata was persisted: re-reading via the store returns it.
	metas, err := ts.cfg.ModelMetas(t.Context(), []string{"or/vision"})
	if err != nil {
		t.Fatalf("ModelMetas: %v", err)
	}
	if metas[0].ContextLength != 200000 {
		t.Errorf("persisted context = %d", metas[0].ContextLength)
	}
}

func TestSetupModelsValidationAndUnconfigured(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)

	// Empty model_ids → 400.
	rec := ts.do(t, "POST", "/api/setup/models", map[string]any{"model_ids": []string{}}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty model_ids: status = %d", rec.Code)
	}

	// Provider not configured → still stores defaults and says so.
	rec = ts.do(t, "POST", "/api/setup/models", map[string]any{"model_ids": []string{"m/one"}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Models []struct {
			ID            string `json:"id"`
			ContextWindow int64  `json:"context_window"`
		} `json:"models"`
		Source   map[string]string `json:"source"`
		Provider string            `json:"provider"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Provider == "ok" {
		t.Fatalf("provider = %q, want a not-configured note", out.Provider)
	}
	if out.Source["m/one"] != "defaults" || out.Models[0].ContextWindow != 131072 {
		t.Fatalf("m/one = %+v source %q", out.Models[0], out.Source["m/one"])
	}
}

func TestSetupModelsProviderOverrides(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)

	// Fake /models endpoint that requires a specific bearer key.
	wantKey := "sk-override"
	ms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+wantKey {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"or/vision","context_length":99999}]}`)
	}))
	t.Cleanup(ms.Close)

	// Stored provider points at a dead server with the wrong key — a request
	// without overrides must fail to reach the provider.
	if _, err := ts.cfg.Update(t.Context(), patchFromMap(t, map[string]any{
		"provider": map[string]any{"base_url": "http://127.0.0.1:1", "api_key": "sk-wrong"},
	})); err != nil {
		t.Fatalf("set provider: %v", err)
	}

	// No overrides → stored (broken) provider is used, defaults come back.
	rec := ts.do(t, "POST", "/api/setup/models", map[string]any{
		"model_ids": []string{"or/vision"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Models []struct {
			ID            string `json:"id"`
			ContextWindow int64  `json:"context_window"`
		} `json:"models"`
		Source   map[string]string `json:"source"`
		Provider string            `json:"provider"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Source["or/vision"] != "defaults" || out.Provider == "ok" {
		t.Fatalf("no overrides: source = %q provider = %q, want defaults from stored provider failure",
			out.Source["or/vision"], out.Provider)
	}

	// Unsaved UI values override the stored provider: the fetch reaches the
	// fake endpoint with the override key and returns provider data.
	rec = ts.do(t, "POST", "/api/setup/models", map[string]any{
		"model_ids": []string{"or/vision"},
		"base_url":  ms.URL,
		"api_key":   wantKey,
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Provider != "ok" || out.Source["or/vision"] != "provider" || out.Models[0].ContextWindow != 99999 {
		t.Fatalf("with overrides: provider = %q source = %q model = %+v",
			out.Provider, out.Source["or/vision"], out.Models[0])
	}

	// Override base URL with an EMPTY api key → the stored key (sk-wrong) is
	// used, the fake endpoint rejects it, defaults come back.
	rec = ts.do(t, "POST", "/api/setup/models", map[string]any{
		"model_ids": []string{"or/vision"},
		"base_url":  ms.URL,
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Source["or/vision"] != "defaults" || out.Provider == "ok" {
		t.Fatalf("empty override key fell back wrong: source = %q provider = %q, want defaults (stored key rejected)",
			out.Source["or/vision"], out.Provider)
	}
}

// ---- GET/PUT /api/setup model metadata ----

func TestSetupExposesModelMetasAndHidesListen(t *testing.T) {
	ts := newTestServer(t, quickProvider{}, false)

	rec := ts.do(t, "PUT", "/api/setup", map[string]any{
		"models": map[string]any{
			"whitelist":          []string{"m", "n"},
			"default_chat_model": "m",
			"default_task_model": "n",
			"metas": []map[string]any{
				{
					"model_id":          "m",
					"context_length":    4242,
					"input_modality":    []string{"text", "image"},
					"output_modality":   []string{"text"},
					"reasoning_efforts": []string{"low"},
					"reasoning_default": "low",
				},
			},
		},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d: %s", rec.Code, rec.Body)
	}

	rec = ts.do(t, "GET", "/api/setup", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", rec.Code, rec.Body)
	}
	// listen moved to the -listen CLI flag; it must not leak into setup.
	if strings.Contains(rec.Body.String(), `"listen"`) {
		t.Fatalf("setup response still exposes listen: %s", rec.Body)
	}
	var out struct {
		Config struct {
			Models struct {
				Metas []struct {
					ModelID       string `json:"model_id"`
					ContextLength int64  `json:"context_length"`
				} `json:"metas"`
			} `json:"models"`
		} `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	byID := map[string]int64{}
	for _, m := range out.Config.Models.Metas {
		byID[m.ModelID] = m.ContextLength
	}
	if byID["m"] != 4242 {
		t.Fatalf("m context = %d, want 4242 (stored meta)", byID["m"])
	}
	if byID["n"] != config.DefaultContextLength {
		t.Fatalf("n context = %d, want the default %d", byID["n"], config.DefaultContextLength)
	}
}

// ---- helpers ----

// patchFromMap builds a config.Patch by round-tripping a generic map through
// JSON, so tests can describe partial updates naturally.
func patchFromMap(t *testing.T, m map[string]any) config.Patch {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal patch: %v", err)
	}
	var p config.Patch
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	return p
}
