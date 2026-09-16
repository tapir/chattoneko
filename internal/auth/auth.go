// Package auth implements optional single-user authentication with HS256 JWT
// bearer tokens. Login compares the submitted credentials in constant time
// against CHATTO_USERNAME / CHATTO_PASSWORD and returns a token with a 90-day
// TTL; the middleware admits "Authorization: Bearer <token>" or, on GET
// requests only, a ?token= query parameter — EventSource streams and <img>
// attachment loads cannot set headers.
//
// Login is required exactly when both environment variables are set (non-empty).
// The plaintext password is never persisted and reaches this package through
// the config store snapshot sourced from the environment at startup, so
// changing auth means setting the env vars and restarting.
//
// The signing key is sha256("chattoneko-jwt:" + username + "\x00" + password):
// tokens survive restarts, and a password change invalidates every outstanding
// token at once. Tokens are stateless — no server-side session store and no
// logout invalidation; clients discard the token.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"chattoneko/internal/config"
)

// TokenTTL is the JWT lifetime.
const TokenTTL = 90 * 24 * time.Hour

// Auth handles login and JWT issuance/validation. All state comes from the
// config store snapshot current at request time.
type Auth struct {
	cfgs    *config.Store
	limiter *loginLimiter
}

// loginLimiter is an in-memory token bucket for login attempts.
//
// ponytail: x/time/rate cannot refund a consumed token — Cancel() only
// restores reservations that have not been served yet — so a successful login
// would still eat the owner's budget. Switch to it if that refund semantic
// ever appears.
type loginLimiter struct {
	mu       sync.Mutex
	tokens   int
	lastTime time.Time
	rate     int // tokens per minute
	burst    int // max burst
}

func (l *loginLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(l.lastTime)
	tokensToAdd := int(elapsed.Minutes() * float64(l.rate))
	// Advance lastTime only when the elapsed window produced a token: advancing
	// it on every attempt resets the window, so attempts more frequent than
	// 1/rate-minute would never refill the bucket.
	if tokensToAdd > 0 {
		l.tokens = min(l.burst, l.tokens+tokensToAdd)
		l.lastTime = now
	}
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

// refund returns one token consumed by allow. Successful logins are refunded so
// legitimate multi-device logins cannot exhaust the owner's budget; failed
// attempts keep consuming it, since they are the brute-force vector.
func (l *loginLimiter) refund() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tokens = min(l.burst, l.tokens+1)
}

// ErrInvalidCreds is returned on failed login.
var ErrInvalidCreds = errors.New("invalid credentials")

// ErrRateLimited is returned on too many login attempts.
var ErrRateLimited = errors.New("too many login attempts")

// New builds an Auth reading credentials live from the config store.
func New(cfgs *config.Store) *Auth {
	return &Auth{
		cfgs:    cfgs,
		limiter: &loginLimiter{tokens: 5, lastTime: time.Now(), rate: 5, burst: 5},
	}
}

// deriveSigningKey turns the configured password into the HS256 key. Any
// password change produces a different key, revoking every token at once.
func deriveSigningKey(username, password string) []byte {
	sum := sha256.Sum256([]byte("chattoneko-jwt:" + username + "\x00" + password))
	return sum[:]
}

// Enabled reports whether auth is on (read live from config).
func (a *Auth) Enabled() bool { return a.cfgs.Get().Auth.Enabled }

// Username returns the configured username.
func (a *Auth) Username() string { return a.cfgs.Get().Auth.Username }

// Login verifies credentials against the env-configured credentials and
// returns a signed JWT (90-day TTL). When auth is disabled, Login always
// succeeds.
func (a *Auth) Login(username, password string) (token string, err error) {
	c := a.cfgs.Get()
	if c.Auth.Enabled {
		if !a.limiter.allow() {
			return "", ErrRateLimited
		}
		// Both comparisons run unconditionally and in constant time so a wrong
		// username costs the same as a wrong password (no user-enumeration
		// timing side channel).
		pwOK := subtle.ConstantTimeCompare([]byte(password), []byte(c.Auth.Password)) == 1
		userOK := subtle.ConstantTimeCompare([]byte(username), []byte(c.Auth.Username)) == 1
		if !pwOK || !userOK {
			return "", ErrInvalidCreds
		}
		// A handful of device logins must not lock the owner out.
		a.limiter.refund()
		return a.issueToken(username, c)
	}
	// Auth disabled: issue a token under a stable throwaway key so clients use
	// one code path. Nothing validates it while disabled.
	return a.issueToken(username, c)
}

func (a *Auth) issueToken(username string, c *config.Config) (string, error) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Subject:   username,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(TokenTTL)),
	}
	pw := c.Auth.Password
	if !c.Auth.Enabled {
		pw = "disabled" // auth off: placeholder key, validation is skipped anyway
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(deriveSigningKey(c.Auth.Username, pw))
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return token, nil
}

// CheckToken reports whether the token is a valid, unexpired JWT signed with
// the key derived from the CURRENT credentials.
func (a *Auth) CheckToken(token string) bool {
	if token == "" {
		return false
	}
	c := a.cfgs.Get()
	if !c.Auth.Enabled {
		return true // auth off: everything is admitted
	}
	if c.Auth.Password == "" {
		return false // auth on but no password set: fail closed
	}
	key := deriveSigningKey(c.Auth.Username, c.Auth.Password)
	_, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
		return key, nil
	},
		// Only the method issued here (no HS384/HS512 downgrade room); exp must
		// be present (v5 validates it only when present, and a signed token
		// without exp would never expire); strict decoding rejects non-canonical
		// base64, whose unused trailing bits would otherwise give a signature a
		// second valid encoding (RFC 4648 §3.5).
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithExpirationRequired(),
		jwt.WithStrictDecoding(),
	)
	return err == nil
}

// ValidateRequest checks the Bearer token, or the ?token= query parameter on
// GET requests. Non-GET requests must carry the Bearer header.
func (a *Auth) ValidateRequest(r *http.Request) bool {
	if !a.Enabled() {
		return true
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		const prefix = "Bearer "
		// The auth scheme is case-insensitive per RFC 7235.
		if len(auth) > len(prefix) && strings.EqualFold(auth[:len(prefix)], prefix) && a.CheckToken(auth[len(prefix):]) {
			return true
		}
	}
	if r.Method == http.MethodGet {
		if token := r.URL.Query().Get("token"); token != "" && a.CheckToken(token) {
			return true
		}
	}
	return false
}

// Middleware blocks /api access when auth is enabled and the request is invalid.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.ValidateRequest(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}
