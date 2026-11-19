package handler

import "net/http"

// HealthPath answers the proxy's health check.
const HealthPath = "/healthz"

// WithHealth answers GET/HEAD /healthz before every other layer: no rate
// limit, no authentication, no IPFS and no JSON. The proxy in front of the
// storage marks it down when this check times out, and then no image on the
// platform loads (theGHub1/docs#201); it has to answer whenever the process
// can still serve, and only then.
func WithHealth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != HealthPath {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("ok\n"))
		}
	})
}
