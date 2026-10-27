package stats

import (
	"io/fs"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnstats"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/webserve"
)

type Config struct {
	Addr    string
	Release string
	Dist    fs.FS
	App     *pgxpool.Pool
	Yield   *pgxpool.Pool
}

func headers(release string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Loyal-Release", release)
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "SAMEORIGIN")
			h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
			h.Set("Strict-Transport-Security", "max-age=63072000")
			next.ServeHTTP(w, r)
		})
	}
}

func apiConfig() huma.Config {
	config := huma.DefaultConfig("Loyal stats API", "1.0.0")
	config.DocsPath = ""
	config.OpenAPIPath = ""
	config.SchemasPath = ""
	config.CreateHooks = nil
	return config
}

func New(cfg Config) http.Handler {
	site := webserve.New(cfg.Dist)
	vaults := newVaultList(cfg.Yield)
	growth := newGrowthMetrics(cfg.Yield)
	r := chi.NewRouter()
	r.Use(headers(cfg.Release))
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Get("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !site.Ready() {
			http.Error(w, "stats build missing", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	earnstats.Register(humachi.New(r, apiConfig()), cfg.App)
	r.Get("/api/earn/vaults", vaults.ServeHTTP)
	r.Head("/api/earn/vaults", vaults.ServeHTTP)
	r.Get("/api/earn/growth-metrics", growth.ServeHTTP)
	r.Handle("/api/*", http.NotFoundHandler())
	r.Handle("/*", site)
	return r
}
