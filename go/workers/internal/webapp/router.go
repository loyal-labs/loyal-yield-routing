package webapp

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/webserve"
)

type Config struct {
	Addr     string
	Upstream *url.URL
	AppHost  string // requests for this host at / go to /app, as apps/web middleware.ts does
	Release  string
	Ready    func() bool
}

func New(cfg Config, deps Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(headers(cfg))
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Get("/readyz", ready(cfg.ready()))

	api := humachi.New(r, apiConfig())
	RegisterAPI(api, deps)
	proxied := forward(cfg.Upstream)
	r.Handle("/api/*", proxied)
	r.Handle("/ingest/*", proxied)
	r.Handle("/*", site(cfg))
	return r
}

func (cfg Config) ready() func() bool {
	if cfg.Ready != nil {
		return cfg.Ready
	}
	return webserve.Ready
}

func ready(ok func() bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if !ok() {
			http.Error(w, "web build missing", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// site serves the SPA, with the host routing apps/web's middleware does
// (middleware.ts:7-22): the app host sends "/" to /app and never
// locale-redirects; every other host is a site host.
func site(cfg Config) http.Handler {
	app := webserve.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfg.AppHost != "" && requestHost(r) == cfg.AppHost {
			if r.URL.Path == "/" {
				http.Redirect(w, r, "/app", http.StatusFound)
				return
			}
			app.ServeHTTP(w, r)
			return
		}
		// The middleware matcher skips paths with a dot (middleware.ts:25),
		// so static files never locale-redirect.
		if !strings.Contains(r.URL.Path, ".") && localeRedirect(w, r) {
			return
		}
		app.ServeHTTP(w, r)
	})
}

// requestHost is the Host header without its port, as middleware.ts:8 reads
// the hostname.
func requestHost(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.Host); err == nil {
		return host
	}
	return r.Host
}

func apiConfig() huma.Config {
	config := huma.DefaultConfig("Loyal API", "1.0.0")
	config.DocsPath = ""
	config.OpenAPIPath = "/api/openapi"
	return config
}

func OpenAPI() *huma.OpenAPI {
	api := humachi.New(chi.NewRouter(), apiConfig())
	RegisterAPI(api, Deps{})
	return api.OpenAPI()
}
