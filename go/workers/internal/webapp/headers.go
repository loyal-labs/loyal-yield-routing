package webapp

import "net/http"

// headers sets what every response carries.
//
// Seed body: the release. G5 adds the CSP (Report-Only) and frame-ancestors
// that apps/web next.config.ts:44-72,150-167 sets.
func headers(cfg Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Loyal-Release", cfg.Release)
			next.ServeHTTP(w, r)
		})
	}
}
