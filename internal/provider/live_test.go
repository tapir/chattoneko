package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLiveUnconfiguredFailsCleanly(t *testing.T) {
	l := NewLive("", "")
	if l.configured() {
		t.Fatal("empty endpoint must be unconfigured")
	}
	_, err := l.StreamChat(context.Background(), nil, nil, GenParams{Model: "m"})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestLiveReconfigureSwapsEndpoint(t *testing.T) {
	// Two servers that each mark which one was hit.
	hit := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			t.Logf("hit %s", name)
		}))
	}
	a := hit("a")
	defer a.Close()
	b := hit("b")
	defer b.Close()

	l := NewLive("", "")
	if l.configured() {
		t.Fatal("should start unconfigured")
	}

	// Configure endpoint A: now usable.
	l.Reconfigure(a.URL, "key")
	if !l.configured() {
		t.Fatal("should be configured after Reconfigure")
	}

	// Switch to endpoint B: underlying client rebuilt (no panic, still configured).
	l.Reconfigure(b.URL, "key")
	if !l.configured() {
		t.Fatal("should remain configured after endpoint swap")
	}

	// Clearing config returns to the unconfigured state.
	l.Reconfigure("", "")
	if l.configured() {
		t.Fatal("clearing endpoint must return to unconfigured")
	}
	if _, err := l.StreamChat(context.Background(), nil, nil, GenParams{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v after clear", err)
	}
}

func TestLiveReconfigureNoopWhenUnchanged(t *testing.T) {
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	defer a.Close()

	l := NewLive(a.URL, "key")
	first := l.inner
	l.Reconfigure(a.URL, "key") // same values: must not rebuild
	if l.inner != first {
		t.Fatal("Reconfigure with unchanged values rebuilt the client")
	}
	l.Reconfigure(a.URL, "other") // key changed: rebuild
	if l.inner == first {
		t.Fatal("Reconfigure with a changed key must rebuild the client")
	}
}
