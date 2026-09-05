// Package auth implements optional single-user authentication with JWT
// bearer tokens: login credentials are compared in constant time against the
// credentials read from the CHATTO_USERNAME / CHATTO_PASSWORD environment
// variables, and a signed JWT (90-day TTL) is returned. The middleware
// admits "Authorization: Bearer <token>" or — on GET requests only, for
// EventSource streams and <img> attachment loads that can't set headers — a
// ?token= query parameter.
//
// Auth is env-var driven: login is required exactly when BOTH environment
// variables are set (non-empty). The password is used as plaintext and is
// never persisted to the database. Credentials are read from the config
// store snapshot, which sources them from the environment at startup;
// changing auth requires setting the env vars and restarting.
//
// The HS256 signing key is derived from the credentials
// (sha256("chattoneko-jwt:" + username + "\x00" + password)): tokens
// survive restarts, and changing the password changes the key, invalidating
// every outstanding token at once. Tokens are stateless — there is no
// server-side session store and no logout invalidation; clients simply
// discard the token.
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

// TokenTTL is the JWT lifetime (90 days).
const TokenTTL = 90 * 24 * time.Hour

// Auth handles login and JWT issuance/validation. All state comes from the
// config store snapshot current at request time.
type Auth struct {
	cfgs    *config.Store
	limiter *loginLimiter
}

// loginLimiter is a simple in-memory token bucket for login attempts.
//
// ponytail: hand-rolled on purpose. x/time/rate cannot refund a consumed
// token — Cancel() only restores reservations that have not been served yet,
// so a successful login would still eat the owner's budget (verified:
// 5 reserve+cancel pairs drain a burst-5 bucket to zero). Revisit only if
// the refund semantic goes away.
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
	// Only consume the elapsed window when it actually produced a token.
	// Advancing lastTime unconditionally starved the bucket: attempts more
	// frequent than 1/rate-minute reset the window each time, so tokens
	// never refilled and login stayed rate-limited forever.
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

// refund returns one token consumed by allow. Successful logins are
// refunded so legitimate multi-device logins cannot exhaust the owner's
// budget; failed attempts keep consuming (they are the brute-force vector).
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
		// Both comparisons run unconditionally and in constant time so a
		// wrong username costs the same as a wrong password (no
		// user-enumeration timing side channel).
		pwOK := subtle.ConstantTimeCompare([]byte(password), []byte(c.Auth.Password)) == 1
		userOK := subtle.ConstantTimeCompare([]byte(username), []byte(c.Auth.Username)) == 1
		if !pwOK || !userOK {
			return "", ErrInvalidCreds
		}
		// Refund the attempt: successful logins must not eat the owner's
		// budget (a handful of device logins would otherwise lock them out).
		a.limiter.refund()
		return a.issueToken(username, c)
	}
	// Auth disabled: hand out a token signed with a stable throwaway key so
	// clients can use one code path. Nothing validates it while disabled.
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
		// Only the method we issue with (no HS384/HS512 downgrade room);
		// exp must be present (v5 validates it only when present, and a
		// signed token without exp would never expire); reject non-canonical
		// base64 so a signature's unused trailing bits cannot be flipped
		// into a second valid encoding (RFC 4648 §3.5).
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithExpirationRequired(),
		jwt.WithStrictDecoding(),
	)
	return err == nil
}

// ValidateRequest checks the Bearer token, or — on GET requests — the
// ?token= query parameter (EventSource streams and <img> attachment loads
// can't set headers). Non-GET requests must carry the Bearer header.
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
