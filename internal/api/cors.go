// CORS for the mobile app: the Capacitor build bundles the SPA and serves it
// from the WebView's own origin, then talks to a user-configured server
// cross-origin. Requests from those origins are allowed; everything else is
// left untouched (the embedded web UI is same-origin and needs no CORS).
package api

import "net/http"

// appOrigins are the origins Capacitor WebViews serve the bundled SPA from:
// Android uses http(s)://localhost, iOS uses capacitor://localhost.
var appOrigins = map[string]bool{
	"http://localhost":      true,
	"https://localhost":     true,
	"capacitor://localhost": true,
}

// corsMiddleware permits cross-origin requests from the mobile app's WebView
// origins. Preflight (OPTIONS) requests never carry credentials/headers, so
// they are answered here before routing (and thus before auth middleware).
// Credentials stay off: the mobile app authenticates with Bearer tokens.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if !appOrigins[origin] {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Add("Vary", "Origin")
		if r.Method == http.MethodOptions {
			// PUT must stay in this list: settings are saved via PUT
			// /api/setup, the app's only PUT route — dropping it makes the
			// preflight fail and mobile settings saves silently no-op.
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			if reqHeaders := r.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
				h.Set("Access-Control-Allow-Headers", reqHeaders)
			} else {
				h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			}
			h.Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
