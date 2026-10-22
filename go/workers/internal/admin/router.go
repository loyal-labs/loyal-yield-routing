package admin

import (
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/webserve"
)

type Config struct {
	Addr    string
	Release string
	Dist    fs.FS
	Auth    Auth
	Deps    *Deps
}

type Mux struct {
	API    huma.API
	Router chi.Router
	Deps   *Deps
}

var families = []func(*Mux){
	registerDapps, registerLibrary, registerCommunities,
	registerJourneys, registerSmartAccounts,
	registerEarn, registerRebalance, registerAutonomousVault, registerEarnMax, registerMetrics,
}

const landingPath = "/earn"

var redirects = map[string]string{
	"/":                            landingPath,
	"/overview":                    landingPath,
	"/vault-integrations/backyard": "/vault-integrations/earn-max",
}

var securityPolicy = strings.Join([]string{
	"default-src 'self'",
	"script-src 'self'",
	"style-src 'self' 'unsafe-inline'",
	"img-src 'self' data: blob: https:",
	"font-src 'self' data:",
	"connect-src 'self'",
	"object-src 'none'",
	"base-uri 'self'",
	"form-action 'self'",
	"frame-ancestors 'none'",
}, "; ")

func headers(release string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Loyal-Release", release)
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "same-origin")
			h.Set("Content-Security-Policy", securityPolicy)
			next.ServeHTTP(w, r)
		})
	}
}

func requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		status := ww.Status()
		if status == 0 {
			status = http.StatusOK
		}
		slog.Info("loyal-admin request", "method", r.Method, "path", r.URL.Path, "status", status, "duration_ms", time.Since(started).Milliseconds())
	})
}

func apiConfig() huma.Config {
	config := huma.DefaultConfig("Loyal admin API", "1.0.0")
	config.DocsPath = ""
	config.OpenAPIPath = ""
	config.CreateHooks = nil
	return config
}

func register(r chi.Router, deps *Deps) huma.API {
	m := &Mux{API: humachi.New(r, apiConfig()), Router: r, Deps: deps}
	for _, family := range families {
		family(m)
	}
	return m.API
}

func New(cfg Config) http.Handler {
	site := webserve.New(cfg.Dist)
	r := chi.NewRouter()
	r.Use(headers(cfg.Release))
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Get("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !site.Ready() {
			http.Error(w, "admin build missing", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	r.Group(func(r chi.Router) {
		r.Use(requestLog, CSRF, cfg.Auth.Gate)
		r.Post("/auth/login", cfg.Auth.Login)
		r.Get("/logout", Logout)
		r.Post("/logout", Logout)
		for from, to := range redirects {
			r.Get(from, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, to, http.StatusFound) })
		}
		register(r, cfg.Deps)
		r.Handle("/api/*", http.NotFoundHandler())
		r.Handle("/*", site)
	})
	return r
}

func OpenAPI() *huma.OpenAPI {
	return register(chi.NewRouter(), nil).OpenAPI()
}
