package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"chattoneko/internal/config"
	"chattoneko/internal/db"
)

// newStore builds a config.Store over an in-memory DB seeded with cfg.
func newStore(t *testing.T, cfg config.Config) *config.Store {
	t.Helper()
	st, err := buildStore(cfg)
	if err != nil {
		t.Fatalf("build store: %v", err)
	}
	return st
}

// buildStore is newStore without t.Fatal so rejection tests can assert errors.
func buildStore(cfg config.Config) (*config.Store, error) {
	sqlDB, err := db.Open(":memory:")
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(sqlDB); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	st, err := config.TestStore(context.Background(), sqlDB, cfg)
	if err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return st, nil
}

// storeWithAuth builds a store holding the plaintext password pw, the way
// env-var driven auth sources it.
func storeWithAuth(t *testing.T, user, pw string) *config.Store {
	t.Helper()
	return newStore(t, config.Config{Auth: config.AuthConfig{
		Enabled:  true,
		Username: user,
		Password: pw,
	}})
}

func newAuth(t *testing.T, st *config.Store) *Auth {
	t.Helper()
	return New(st)
}

// currentKey derives the signing key for the store's current snapshot.
func currentKey(a *Auth) []byte {
	c := a.cfgs.Get()
	pw := c.Auth.Password
	if !c.Auth.Enabled {
		pw = "disabled"
	}
	return deriveSigningKey(c.Auth.Username, pw)
}

func TestLoginSuccess(t *testing.T) {
	a := newAuth(t, storeWithAuth(t, "alice", "s3cret"))
	token, err := a.Login("alice", "s3cret")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if token == "" {
		t.Fatal("empty token")
	}
	if !a.CheckToken(token) {
		t.Fatal("token not valid after login")
	}

	// Claims: subject is the username, expiry is ~90 days out.
	parsed, err := jwt.Parse(token, func(tok *jwt.Token) (any, error) {
		return currentKey(a), nil
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatal("unexpected claims type")
	}
	if sub, _ := claims.GetSubject(); sub != "alice" {
		t.Fatalf("sub = %q", sub)
	}
	exp, err := claims.GetExpirationTime()
	if err != nil || exp == nil {
		t.Fatalf("no exp claim: %v", err)
	}
	if ttl := time.Until(exp.Time); ttl < TokenTTL-time.Hour || ttl > TokenTTL {
		t.Fatalf("token ttl = %v, want ~%v", ttl, TokenTTL)
	}
}

func TestLoginBadCreds(t *testing.T) {
	a := newAuth(t, storeWithAuth(t, "alice", "s3cret"))
	if _, err := a.Login("alice", "wrong"); err == nil {
		t.Fatal("accepted wrong password")
	}
	if _, err := a.Login("bob", "s3cret"); err == nil {
		t.Fatal("accepted wrong username")
	}
}

func TestLoginRateLimit(t *testing.T) {
	a := newAuth(t, storeWithAuth(t, "alice", "s3cret"))
	for i := 0; i < 5; i++ {
		_, _ = a.Login("x", "y")
	}
	if _, err := a.Login("x", "y"); err != ErrRateLimited {
		t.Fatalf("not rate limited: %v", err)
	}
}

// Successful logins refund their token: legitimate multi-device logins must
// not exhaust the owner's budget; only failures consume it.
func TestLoginRateLimitIgnoresSuccesses(t *testing.T) {
	a := newAuth(t, storeWithAuth(t, "alice", "s3cret"))
	for i := 0; i < 10; i++ {
		if _, err := a.Login("alice", "s3cret"); err != nil {
			t.Fatalf("successful login %d: %v", i, err)
		}
	}
	// Budget intact: a failed attempt still proceeds (and consumes).
	if _, err := a.Login("alice", "wrong"); err == ErrRateLimited {
		t.Fatal("rate limited after only successful logins")
	}
	// And failures still exhaust the bucket as before.
	for i := 0; i < 4; i++ {
		_, _ = a.Login("alice", "wrong")
	}
	if _, err := a.Login("alice", "wrong"); err != ErrRateLimited {
		t.Fatalf("not rate limited after failures: %v", err)
	}
}

func TestLoginDisabledIsOpen(t *testing.T) {
	a := newAuth(t, newStore(t, config.Config{Auth: config.AuthConfig{Enabled: false}}))
	if _, err := a.Login("any", "thing"); err != nil {
		t.Fatalf("disabled auth must be open: %v", err)
	}
}

// Auth can only be enabled with a non-empty username AND password; a config
// missing either must be rejected loudly when written.
func TestAuthEnabledRequiresCredentials(t *testing.T) {
	for _, cfg := range []config.Config{
		{Auth: config.AuthConfig{Enabled: true, Username: "u", Password: ""}},
		{Auth: config.AuthConfig{Enabled: true, Username: "", Password: "p"}},
		{Auth: config.AuthConfig{Enabled: true, Username: "  ", Password: "p"}},
	} {
		if _, err := buildStore(cfg); err == nil {
			t.Fatalf("accepted incomplete auth config %+v", cfg.Auth)
		}
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	a := newAuth(t, storeWithAuth(t, "u", "p"))
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Subject:   "u",
		IssuedAt:  jwt.NewNumericDate(now.Add(-2 * TokenTTL)),
		ExpiresAt: jwt.NewNumericDate(now.Add(-TokenTTL)),
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(currentKey(a))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if a.CheckToken(tok) {
		t.Fatal("expired token accepted")
	}
}

func TestForeignTokenRejected(t *testing.T) {
	a := newAuth(t, storeWithAuth(t, "u", "p"))
	// A token signed under different credentials.
	other := newAuth(t, storeWithAuth(t, "u", "different"))
	tok, err := other.Login("u", "different")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if a.CheckToken(tok) {
		t.Fatal("token signed by another key accepted")
	}
}

func TestNoneAlgRejected(t *testing.T) {
	a := newAuth(t, storeWithAuth(t, "u", "p"))
	tok, err := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.RegisteredClaims{
		Subject:   "u",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if a.CheckToken(tok) {
		t.Fatal("alg=none token accepted")
	}
}

// The accepted method is pinned to HS256: a token signed with the right key
// under another HMAC variant must not pass.
func TestHS384Rejected(t *testing.T) {
	a := newAuth(t, storeWithAuth(t, "u", "p"))
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS384, jwt.RegisteredClaims{
		Subject:   "u",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).SignedString(currentKey(a))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if a.CheckToken(tok) {
		t.Fatal("HS384 token accepted")
	}
}

// exp is mandatory: v5 validates it only when present, and a signed token
// without exp would otherwise never expire.
func TestMissingExpRejected(t *testing.T) {
	a := newAuth(t, storeWithAuth(t, "u", "p"))
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject: "u",
	}).SignedString(currentKey(a))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if a.CheckToken(tok) {
		t.Fatal("token without exp accepted")
	}
}

// base64url with non-zero unused bits in the final group decodes to the
// same signature bytes (lenient decoding); strict decoding must reject the
// malleable variant.
func TestMalleableSignatureRejected(t *testing.T) {
	a := newAuth(t, storeWithAuth(t, "u", "p"))
	token, err := a.Login("u", "p")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token parts = %d", len(parts))
	}
	sig := parts[2]
	raw, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		t.Fatal(err)
	}
	// Canonical encodings keep the unused trailing bits zero, so flipping
	// them (idx|1) changes the text but not the decoded bytes.
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alpha, sig[len(sig)-1])
	mangledSig := sig[:len(sig)-1] + string(alpha[last|1])
	if raw2, err := base64.RawURLEncoding.DecodeString(mangledSig); err != nil || !bytes.Equal(raw, raw2) {
		t.Fatalf("test setup: mangled signature decodes differently (%v)", err)
	}
	mangled := parts[0] + "." + parts[1] + "." + mangledSig
	if a.CheckToken(mangled) {
		t.Fatal("non-canonical base64 token accepted")
	}
}

func TestValidateRequestPaths(t *testing.T) {
	a := newAuth(t, storeWithAuth(t, "u", "p"))
	token, err := a.Login("u", "p")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	// Bearer header path.
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	if !a.ValidateRequest(r) {
		t.Fatal("bearer token rejected")
	}

	// Query param ?token=: accepted on GET routes (EventSource and <img>
	// can't set headers); non-GET requests still require the Bearer header.
	r2 := httptest.NewRequest("GET", "/api/chats/abc/stream?token="+token, nil)
	if !a.ValidateRequest(r2) {
		t.Fatal("token rejected on stream route")
	}
	r2b := httptest.NewRequest("GET", "/api/chats?token="+token, nil)
	if !a.ValidateRequest(r2b) {
		t.Fatal("token rejected on non-stream GET route")
	}
	r2c := httptest.NewRequest("POST", "/api/chats?token="+token, nil)
	if a.ValidateRequest(r2c) {
		t.Fatal("query token accepted on a POST route")
	}

	// Invalid: no credentials, garbage token.
	r3 := httptest.NewRequest("GET", "/", nil)
	if a.ValidateRequest(r3) {
		t.Fatal("unauthenticated request validated")
	}
	r4 := httptest.NewRequest("GET", "/", nil)
	r4.Header.Set("Authorization", "Bearer garbage")
	if a.ValidateRequest(r4) {
		t.Fatal("garbage token validated")
	}

	// The auth scheme is case-insensitive per RFC 7235.
	r5 := httptest.NewRequest("GET", "/", nil)
	r5.Header.Set("Authorization", "bearer "+token)
	if !a.ValidateRequest(r5) {
		t.Fatal("lowercase bearer scheme rejected")
	}
}

func TestValidateRequestDisabledIsOpen(t *testing.T) {
	a := newAuth(t, newStore(t, config.Config{Auth: config.AuthConfig{Enabled: false}}))
	if !a.ValidateRequest(httptest.NewRequest("GET", "/", nil)) {
		t.Fatal("disabled auth must admit everything")
	}
}

// A disabled store admits everything, regardless of path or token.
func TestDisabledAdmitsEverything(t *testing.T) {
	a := newAuth(t, newStore(t, config.Config{Auth: config.AuthConfig{Enabled: false}}))
	r := httptest.NewRequest("POST", "/api/chats", nil) // no token at all
	if !a.ValidateRequest(r) {
		t.Fatal("disabled auth must admit even unauthenticated requests")
	}
	if _, err := a.Login("anyone", "whatever"); err != nil {
		t.Fatalf("disabled auth must always issue a token: %v", err)
	}
}
