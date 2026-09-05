package provider

import (
	"context"
	"errors"
	"sync"
)

// ErrNotConfigured is returned when the provider endpoint/key have not been
// configured yet (fresh install before setup completes).
var ErrNotConfigured = errors.New("provider is not configured yet (set the provider base URL and API key)")

// Live is a Provider whose upstream endpoint can be swapped at runtime: the
// chat machinery constructs it once at startup and keeps using it while
// config changes re-dial the underlying client via Reconfigure. When the
// endpoint is not (yet) configured, StreamChat fails with ErrNotConfigured
// instead of panicking or dialing an empty URL.
type Live struct {
	mu    sync.RWMutex
	inner Provider
	base  string
	key   string
}

var _ Provider = (*Live)(nil)

// NewLive builds the live provider wrapper. Empty baseURL/apiKey leave it
// unconfigured until Reconfigure is called with real values.
func NewLive(baseURL, apiKey string) *Live {
	l := &Live{}
	l.Reconfigure(baseURL, apiKey)
	return l
}

// Reconfigure swaps the underlying client when the endpoint changed. A
// no-op when baseURL and apiKey are unchanged. Empty values leave (or reset
// to) the unconfigured state.
func (l *Live) Reconfigure(baseURL, apiKey string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if baseURL == l.base && apiKey == l.key {
		return
	}
	l.base, l.key = baseURL, apiKey
	if baseURL == "" || apiKey == "" {
		l.inner = nil
		return
	}
	l.inner = newChatCompletionsProvider(baseURL, apiKey)
}

// configured reports whether an endpoint is currently dialed. Test-only.
func (l *Live) configured() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.inner != nil
}

// StreamChat proxies to the current underlying provider.
func (l *Live) StreamChat(ctx context.Context, msgs []Message, tools []Tool, p GenParams) (*EventStream, error) {
	l.mu.RLock()
	inner := l.inner
	l.mu.RUnlock()
	if inner == nil {
		return nil, ErrNotConfigured
	}
	return inner.StreamChat(ctx, msgs, tools, p)
}
