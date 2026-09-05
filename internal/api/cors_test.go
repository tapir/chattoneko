package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCORSPreflightAllowedOrigin(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("preflight must not reach the inner handler")
	})
	req := httptest.NewRequest("OPTIONS", "/api/chats", nil)
	req.Header.Set("Origin", "http://localhost")
	req.Header.Set("Access-Control-Request-Headers", "content-type, authorization")
	rec := httptest.NewRecorder()
	corsMiddleware(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost" {
		t.Fatalf("ACAO = %q, want http://localhost", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "content-type, authorization" {
		t.Fatalf("ACAH = %q, want echo of requested headers", got)
	}
}

// Regression: the preflight must advertise every method the app uses —
// notably PUT (settings save). A missing method makes the browser block the
// actual request, so mobile settings saves fail while everything else works.
func TestCORSPreflightAllowsPut(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("preflight must not reach the inner handler")
	})
	req := httptest.NewRequest("OPTIONS", "/api/setup", nil)
	req.Header.Set("Origin", "https://localhost")
	req.Header.Set("Access-Control-Request-Method", "PUT")
	rec := httptest.NewRecorder()
	corsMiddleware(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	methods := rec.Header().Get("Access-Control-Allow-Methods")
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		if !strings.Contains(methods, m) {
			t.Fatalf("Access-Control-Allow-Methods = %q, missing %s", methods, m)
		}
	}
}

func TestCORSAllowedOriginOnRequest(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest("GET", "/api/meta", nil)
	req.Header.Set("Origin", "capacitor://localhost")
	rec := httptest.NewRecorder()
	corsMiddleware(next).ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "capacitor://localhost" {
		t.Fatalf("ACAO = %q, want capacitor://localhost", got)
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("Vary = %q, want Origin", got)
	}
}

func TestCORSDisallowedOriginUntouched(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	// Preflight from an arbitrary origin must fall through to routing (405
	// via the mux in production; here the stub proves it wasn't answered).
	req := httptest.NewRequest("OPTIONS", "/api/chats", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	corsMiddleware(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("disallowed-origin OPTIONS status = %d, want fallthrough", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("ACAO set for disallowed origin: %q", got)
	}
}
