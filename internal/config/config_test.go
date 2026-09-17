package config

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"chattoneko/internal/db"
)

// newDB opens a migrated in-memory database.
func newDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return sqlDB
}

func TestSeedDefaultsOnEmptyTable(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	s, err := NewStore(ctx, h)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	c := s.Get()
	if c.SystemPrompt == "" {
		t.Error("SystemPrompt empty, want the embedded default")
	}
	if c.Limits.UploadMaxFileBytes != DefaultUploadMaxFileBytes {
		t.Errorf("UploadMaxFileBytes = %d", c.Limits.UploadMaxFileBytes)
	}
	if c.Limits.MaxToolIterations != DefaultMaxToolIterations {
		t.Errorf("MaxToolIterations = %d", c.Limits.MaxToolIterations)
	}
	if c.Limits.MCPCallTimeoutSeconds != DefaultMCPCallTimeoutSeconds {
		t.Errorf("MCPCallTimeoutSeconds = %d", c.Limits.MCPCallTimeoutSeconds)
	}
	if c.ImageQuantization != DefaultImageQuantization {
		t.Errorf("ImageQuantization = %v", c.ImageQuantization)
	}
	// Auth is env-var driven; with no env vars set (as in tests) it is disabled
	// and carries no username. It is NOT seeded from the database.
	if c.Auth.Enabled {
		t.Error("auth must be disabled when the env vars are not set")
	}
	if c.Auth.Username != "" || c.Auth.Password != "" {
		t.Errorf("Auth = %+v, want zero value without env vars", c.Auth)
	}
	// Everything else empty/null until configured.
	if c.Provider.BaseURL != "" || c.Provider.APIKey != "" {
		t.Error("provider fields must default to empty")
	}
	if len(c.Models.Whitelist) != 0 || c.Models.DefaultChatModel != "" || c.Models.DefaultTaskModel != "" {
		t.Error("model fields must default to empty")
	}
	if len(c.MCPServers) != 0 {
		t.Error("mcp_servers must default to empty")
	}
	if c.Complete() {
		t.Error("fresh default config must be incomplete")
	}
}

func TestSeedRunsOnlyOnce(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	s, err := NewStore(ctx, h)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := s.Update(ctx, Patch{SystemPrompt: ptr("custom prompt")}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// Reopening must NOT reset the stored value.
	s2, err := NewStore(ctx, h)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := s2.Get().SystemPrompt; got != "custom prompt" {
		t.Fatalf("SystemPrompt = %q after reopen, want %q", got, "custom prompt")
	}
}

// Auth is driven by CHATTO_USERNAME / CHATTO_PASSWORD. Login is required exactly
// when BOTH are set; the password stays plaintext and is never written to the
// config table.
func TestAuthFromEnv(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	t.Setenv(EnvUsername, "envuser")
	t.Setenv(EnvPassword, "envpass")
	s, err := NewStore(ctx, h)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	c := s.Get()
	if !c.Auth.Enabled {
		t.Fatal("auth must be enabled when both env vars are set")
	}
	if c.Auth.Username != "envuser" || c.Auth.Password != "envpass" {
		t.Errorf("Auth = %+v", c.Auth)
	}
	// Auth must NOT be persisted to the config table.
	var n int
	if err := h.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM config WHERE key IN ('auth_enabled','username','password_hash')`).Scan(&n); err != nil {
		t.Fatalf("query config: %v", err)
	}
	if n != 0 {
		t.Errorf("auth rows persisted to config table: %d", n)
	}
}

// Missing EITHER variable disables login entirely.
func TestAuthFromEnvRequiresBoth(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T){
		"only username": func(t *testing.T) { t.Setenv(EnvUsername, "u") },
		"only password": func(t *testing.T) { t.Setenv(EnvPassword, "p") },
		"neither":       func(*testing.T) {},
	} {
		t.Run(name, func(t *testing.T) {
			setup(t)
			h := newDB(t)
			ctx := context.Background()
			s, err := NewStore(ctx, h)
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			if s.Get().Auth.Enabled {
				t.Fatal("auth must be disabled when the env vars are not both set")
			}
		})
	}
}

// Whitespace-only values count as unset.
func TestAuthFromEnvTrims(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	t.Setenv(EnvUsername, "  user  ")
	t.Setenv(EnvPassword, "   ")
	s, err := NewStore(ctx, h)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if s.Get().Auth.Enabled {
		t.Fatal("a whitespace-only password must disable auth")
	}
}

func TestUpdatePartialPatch(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	s, _ := NewStore(ctx, h)

	if _, err := s.Update(ctx, Patch{
		Provider: &ProviderPatch{BaseURL: ptr("https://p.example/api"), APIKey: ptr("sk-test")},
		Models: &ModelsPatch{
			Whitelist:        &[]string{"a", "b"},
			DefaultChatModel: ptr("a"),
			DefaultTaskModel: ptr("b"),
		},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	c := s.Get()
	if !c.Complete() {
		t.Fatal("config should be complete after provider+models set")
	}
	// Auth is env-driven and untouched by a provider/models patch.
	if c.Auth.Enabled {
		t.Error("auth should stay disabled (no env vars in test)")
	}
}

func TestToolDefaultsRoundTrip(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	s, _ := NewStore(ctx, h)
	m := map[string]bool{"a": true, "b": false}
	if _, err := s.Update(ctx, Patch{ToolDefaults: &m}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// Survives a reload from the config table.
	if err := s.reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := s.Get().ToolDefaults; len(got) != 2 || !got["a"] || got["b"] {
		t.Fatalf("tool_defaults = %v, want map[a:true b:false]", got)
	}
}

// An explicit on has to survive a reload, and a database written before the
// key existed reads as off — the default.
func TestImageQuantizationRoundTrip(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	s, err := NewStore(ctx, h)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if s.Get().ImageQuantization {
		t.Fatal("a fresh config must not quantize")
	}

	if _, err := s.Update(ctx, Patch{ImageQuantization: ptr(true)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := s.reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !s.Get().ImageQuantization {
		t.Fatal("on did not survive the reload")
	}

	if _, err := h.ExecContext(ctx, `DELETE FROM config WHERE key = 'image_quantization'`); err != nil {
		t.Fatalf("delete row: %v", err)
	}
	if err := s.reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if s.Get().ImageQuantization {
		t.Fatal("an absent row must read as off")
	}
}

func TestUpdateSanitizesWhitelist(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	s, _ := NewStore(ctx, h)
	_, err := s.Update(ctx, Patch{Models: &ModelsPatch{
		Whitelist:        &[]string{"a", "", "a", "b"},
		DefaultChatModel: ptr("chat-model"),
	}})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	c := s.Get()
	// Dups + empty dropped.
	if strings.Join(c.Models.Whitelist, ",") != "a,b" {
		t.Fatalf("whitelist = %v, want [a b]", c.Models.Whitelist)
	}
	// The designated chat model is NOT whitelisted, so it is cleared rather
	// than silently auto-added.
	if c.Models.DefaultChatModel != "" {
		t.Fatalf("default chat model = %q, want cleared", c.Models.DefaultChatModel)
	}
}

func TestUpdateSanitizesMCPServers(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	s, _ := NewStore(ctx, h)
	_, err := s.Update(ctx, Patch{MCPServers: &[]MCPServerConfig{
		{Name: "ok", Transport: "http", URL: "https://mcp.example"},
		{Name: "", Transport: "http", URL: "https://x"},                  // dropped: no name
		{Name: "ok", Transport: "http", URL: "https://dup"},              // dropped: duplicate
		{Name: "bad", Transport: "carrier-pigeon"},                       // dropped: bad transport
		{Name: "std", Transport: "http", URL: " https://mcp.example/x "}, // trimmed url
		{Name: "wsurl", Transport: "http", URL: "   "},                   // dropped: whitespace url
	}})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	got := s.Get().MCPServers
	if len(got) != 2 || got[0].Name != "ok" || got[1].Name != "std" || got[1].URL != "https://mcp.example/x" {
		t.Fatalf("servers = %+v", got)
	}
}

func TestSubscribersNotified(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	s, _ := NewStore(ctx, h)
	var calls int
	var lastPrompt string
	s.Subscribe(func(c *Config) {
		calls++
		lastPrompt = c.SystemPrompt
	})
	if _, err := s.Update(ctx, Patch{SystemPrompt: ptr("notify me")}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if calls != 1 || lastPrompt != "notify me" {
		t.Fatalf("calls=%d prompt=%q", calls, lastPrompt)
	}
	// A failed write must not notify.
	if _, err := s.forceSet(ctx, Config{Auth: AuthConfig{Enabled: true, Username: "u"}}); err == nil {
		t.Fatal("expected a validation error for auth enabled without a password")
	}
	if calls != 1 {
		t.Fatalf("calls=%d after failed write", calls)
	}
}

func TestModelMetasDefaultsAndRoundTrip(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	s, _ := NewStore(ctx, h)

	// Unknown id → defaults.
	metas, err := s.ModelMetas(ctx, []string{"x/unknown"})
	if err != nil {
		t.Fatalf("ModelMetas: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("metas = %v", metas)
	}
	m := metas[0]
	if m.ContextLength != DefaultContextLength {
		t.Errorf("ContextLength = %d", m.ContextLength)
	}
	if strings.Join(m.InputModality, ",") != "text" {
		t.Errorf("input modality = %v", m.InputModality)
	}
	if strings.Join(m.ReasoningEfforts, ",") != "max,xhigh,high,medium,low,minimal,none" || m.ReasoningDefault != "medium" {
		t.Errorf("reasoning = %v / %q", m.ReasoningEfforts, m.ReasoningDefault)
	}

	// Upsert and read back.
	err = s.UpsertModelMetas(ctx, []ModelMeta{{
		ModelID:          "x/vision",
		InputModality:    []string{"text", "image"},
		ContextLength:    200000,
		ReasoningEfforts: []string{"minimal", "maximal"},
		ReasoningDefault: "maximal",
	}})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	metas, err = s.ModelMetas(ctx, []string{"x/vision", "x/unknown"})
	if err != nil {
		t.Fatalf("ModelMetas: %v", err)
	}
	if metas[0].ContextLength != 200000 || metas[0].ReasoningDefault != "maximal" {
		t.Errorf("stored meta = %+v", metas[0])
	}
	if strings.Join(metas[0].InputModality, ",") != "text,image" {
		t.Errorf("input modality = %v", metas[0].InputModality)
	}
	// Order + defaults for the second id.
	if metas[1].ModelID != "x/unknown" || metas[1].ContextLength != DefaultContextLength {
		t.Errorf("second meta = %+v", metas[1])
	}
}

func TestSanitizeMeta(t *testing.T) {
	// Invalid modalities filtered, empty → ["text"].
	m := ModelMeta{ModelID: "a", InputModality: []string{"IMAGE", "bogus"}}
	SanitizeMeta(&m)
	if strings.Join(m.InputModality, ",") != "image" {
		t.Errorf("input = %v", m.InputModality)
	}
	m = ModelMeta{ModelID: "a"}
	SanitizeMeta(&m)
	if strings.Join(m.InputModality, ",") != "text" {
		t.Errorf("input = %v", m.InputModality)
	}

	// Bad context → default; default effort not in list → 2nd element.
	m = ModelMeta{ModelID: "b", ContextLength: 0, ReasoningEfforts: []string{"only"}, ReasoningDefault: "nope"}
	SanitizeMeta(&m)
	if m.ContextLength != DefaultContextLength {
		t.Errorf("context = %d", m.ContextLength)
	}
	if m.ReasoningDefault != "only" {
		t.Errorf("default = %q", m.ReasoningDefault)
	}

	m = ModelMeta{ModelID: "c", ReasoningEfforts: []string{"a", "b", "c"}, ReasoningDefault: "zzz"}
	SanitizeMeta(&m)
	if m.ReasoningDefault != "b" {
		t.Errorf("default = %q, want the 2nd element b", m.ReasoningDefault)
	}
}

func TestUpdateModelMetas(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	s, _ := NewStore(ctx, h)

	_, err := s.Update(ctx, Patch{Models: &ModelsPatch{
		Whitelist: &[]string{"a", "b"},
		Metas: &[]ModelMeta{
			{ModelID: "a", ContextLength: 111},
			{ModelID: "b", ContextLength: 222},
		},
	}})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	metas, err := s.ModelMetas(ctx, []string{"a", "b"})
	if err != nil {
		t.Fatalf("ModelMetas: %v", err)
	}
	if metas[0].ContextLength != 111 || metas[1].ContextLength != 222 {
		t.Fatalf("metas = %+v", metas)
	}

	// Shrinking the whitelist prunes the dropped model's metadata.
	if _, err := s.Update(ctx, Patch{Models: &ModelsPatch{Whitelist: &[]string{"a"}}}); err != nil {
		t.Fatalf("shrink: %v", err)
	}
	metas, _ = s.ModelMetas(ctx, []string{"b"})
	if metas[0].ContextLength != DefaultContextLength {
		t.Fatalf("b metadata should be pruned, got %+v", metas[0])
	}
}

// Header values round-trip through the setup API (GET exposes them, PUT carries
// them back verbatim), so the patch is authoritative: an empty value clears the
// header instead of keeping a stored one.
func TestUpdateClearsEmptyMCPHeaderValues(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	s, _ := NewStore(ctx, h)

	_, err := s.Update(ctx, Patch{MCPServers: &[]MCPServerConfig{
		{Name: "s", Transport: "http", URL: "https://x", Headers: map[string]string{"Authorization": "Bearer secret", "X-Keep": "v"}},
	}})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Clearing a value drops that header; the other one survives.
	_, err = s.Update(ctx, Patch{MCPServers: &[]MCPServerConfig{
		{Name: "s", Transport: "http", URL: "https://x", Headers: map[string]string{"Authorization": "", "X-Keep": "v"}},
	}})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	got := s.Get().MCPServers
	if len(got) != 1 || len(got[0].Headers) != 1 || got[0].Headers["X-Keep"] != "v" {
		t.Fatalf("headers = %+v", got)
	}
}

// Header names must be valid HTTP field names and values get trimmed: an invalid
// name would fail the MCP request at call time with a confusing error, so it is
// dropped at config time instead.
func TestUpdateSanitizesMCPHeaders(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	s, _ := NewStore(ctx, h)

	_, err := s.Update(ctx, Patch{MCPServers: &[]MCPServerConfig{
		{Name: "s", Transport: "http", URL: "https://x", Headers: map[string]string{
			"Authorization": "  Bearer secret ", // trimmed
			"bad name":      "v",                // dropped: invalid header name
			"":              "v",                // dropped: empty name
			"X-Ok":          "  ",               // dropped: whitespace-only value
		}},
	}})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	got := s.Get().MCPServers
	if len(got) != 1 {
		t.Fatalf("servers = %+v", got)
	}
	if len(got[0].Headers) != 1 || got[0].Headers["Authorization"] != "Bearer secret" {
		t.Fatalf("headers = %+v", got[0].Headers)
	}
}

func ptr[T any](v T) *T { return &v }

func TestVisionModelPatchAndSanitize(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	s, _ := NewStore(ctx, h)

	// The vision designation requires image input (sanitizeDesignated).
	vision := []ModelMeta{{ModelID: "v", InputModality: []string{"text", "image"}}}
	if _, err := s.Update(ctx, Patch{Models: &ModelsPatch{
		Whitelist:          &[]string{"a", "v"},
		DefaultChatModel:   ptr("a"),
		DefaultTaskModel:   ptr("a"),
		DefaultVisionModel: ptr("v"),
		Metas:              &vision,
	}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := s.Get().Models.DefaultVisionModel; got != "v" {
		t.Fatalf("vision model = %q, want %q", got, "v")
	}
	// Persistence across reopens.
	s2, err := NewStore(ctx, h)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := s2.Get().Models.DefaultVisionModel; got != "v" {
		t.Fatalf("vision model after reopen = %q", got)
	}

	// Dropping the vision model from the whitelist clears the designation.
	if _, err := s.Update(ctx, Patch{Models: &ModelsPatch{Whitelist: &[]string{"a"}}}); err != nil {
		t.Fatalf("Update 2: %v", err)
	}
	if got := s.Get().Models.DefaultVisionModel; got != "" {
		t.Errorf("vision model after whitelist removal = %q, want empty", got)
	}

	// The vision model is optional: it does not make a config complete.
	_, err = s.Update(ctx, Patch{Models: &ModelsPatch{
		Whitelist:          &[]string{"a", "v"},
		DefaultChatModel:   ptr("a"),
		DefaultTaskModel:   ptr(""),
		DefaultVisionModel: ptr("v"),
		Metas:              &vision,
	}})
	if err != nil {
		t.Fatalf("Update 3: %v", err)
	}
	if s.Complete() {
		t.Error("config complete without a task model: the vision model must not substitute for it")
	}
}

// A designated model that cannot do its job is cleared, not rejected: the save
// lands in the same state as one with no designation at all, so Complete()
// reports setup as unfinished and the settings overlay stays forced open until
// the user picks a model that fits the role.
func TestUpdateClearsDesignatedModelWithoutModality(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	s, err := NewStore(ctx, h)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	update := func(p Patch) *Config {
		t.Helper()
		c, err := s.Update(ctx, p)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		return c
	}
	// Provider set, so only the designations decide Complete().
	update(Patch{Provider: &ProviderPatch{BaseURL: ptr("https://p.example"), APIKey: ptr("k")}})
	whitelist := []string{"m", "vision", "pdf"}
	stored := []ModelMeta{
		{ModelID: "vision", InputModality: []string{"image"}},
		{ModelID: "pdf", InputModality: []string{"text", "image", "file"}},
	}
	update(Patch{Models: &ModelsPatch{Whitelist: &whitelist, Metas: &stored}})

	// An image-only model can't be the chat or the task model.
	c := update(Patch{Models: &ModelsPatch{DefaultChatModel: ptr("vision"), DefaultTaskModel: ptr("vision")}})
	if c.Models.DefaultChatModel != "" || c.Models.DefaultTaskModel != "" {
		t.Errorf("designations = %q/%q, want both cleared", c.Models.DefaultChatModel, c.Models.DefaultTaskModel)
	}
	if c.Complete() {
		t.Error("config complete without a chat/task model")
	}
	if got := s.Get().Models; got.DefaultChatModel != "" || got.DefaultTaskModel != "" {
		t.Errorf("stored designations = %+v, want cleared", got)
	}

	// Same for the vision role, which needs a chat model with image input.
	if c = update(Patch{Models: &ModelsPatch{DefaultVisionModel: ptr("audio")}}); c.Models.DefaultVisionModel != "" {
		t.Errorf("vision model = %q, want cleared", c.Models.DefaultVisionModel)
	}
	if c = update(Patch{Models: &ModelsPatch{DefaultVisionModel: ptr("vision")}}); c.Models.DefaultVisionModel != "vision" {
		t.Errorf("vision model = %q, want vision", c.Models.DefaultVisionModel)
	}

	// The document role needs "file" input — image input does NOT imply it,
	// plenty of models see pictures but refuse a PDF.
	if c = update(Patch{Models: &ModelsPatch{DefaultDocumentModel: ptr("vision")}}); c.Models.DefaultDocumentModel != "" {
		t.Errorf("document model = %q, want cleared", c.Models.DefaultDocumentModel)
	}
	if c = update(Patch{Models: &ModelsPatch{DefaultDocumentModel: ptr("pdf")}}); c.Models.DefaultDocumentModel != "pdf" {
		t.Errorf("document model = %q, want pdf", c.Models.DefaultDocumentModel)
	}

	// The audio models are free-standing ids, not whitelist pointers: they are
	// called through routes no chat metadata describes, so nothing validates
	// them and an id outside the whitelist survives (sanitizeWhitelist must not
	// clear them). The voice is trimmed; blank stays blank and means "the
	// provider's own default".
	c = update(Patch{Models: &ModelsPatch{
		DefaultTranscriptionModel: ptr("whisper-large"),
		DefaultSpeechModel:        ptr("tts-1"),
		SpeechVoice:               ptr("  alloy  "),
	}})
	if c.Models.DefaultTranscriptionModel != "whisper-large" || c.Models.DefaultSpeechModel != "tts-1" || c.Models.SpeechVoice != "alloy" {
		t.Errorf("audio = %q/%q/%q, want whisper-large/tts-1/alloy",
			c.Models.DefaultTranscriptionModel, c.Models.DefaultSpeechModel, c.Models.SpeechVoice)
	}
	if got := strings.Join(c.Models.Whitelist, ","); got != "m,vision,pdf" {
		t.Errorf("whitelist = %q, want the audio ids kept out of it", got)
	}

	// Metas sent in the same patch win over the stored rows: adding text input
	// to the card and flagging it as chat saves in one go.
	fixed := []ModelMeta{{ModelID: "vision", InputModality: []string{"text", "image"}}}
	c = update(Patch{Models: &ModelsPatch{DefaultChatModel: ptr("vision"), DefaultTaskModel: ptr("m"), Metas: &fixed}})
	if c.Models.DefaultChatModel != "vision" {
		t.Errorf("chat model = %q, want vision", c.Models.DefaultChatModel)
	}
	if !c.Complete() {
		t.Error("config should be complete once every role has a model that fits")
	}

	// The optional designations survive a reopen.
	s2, err := NewStore(ctx, h)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := s2.Get().Models; got.DefaultDocumentModel != "pdf" || got.DefaultTranscriptionModel != "whisper-large" ||
		got.DefaultSpeechModel != "tts-1" || got.SpeechVoice != "alloy" {
		t.Errorf("stored document/audio = %+v, want pdf + whisper-large/tts-1/alloy", got)
	}
}
