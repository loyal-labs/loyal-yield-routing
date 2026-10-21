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
	AppHost  string
	Release  string
	Ready    func() bool
}

func New(cfg Config) http.Handler {
	r := chi.NewRouter()
	r.Use(headers(cfg))
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Get("/readyz", ready(cfg.ready()))

	api := humachi.New(r, apiConfig())
	RegisterAPI(api)
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
		if !strings.Contains(r.URL.Path, ".") && localeRedirect(w, r) {
			return
		}
		app.ServeHTTP(w, r)
	})
}

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
	RegisterAPI(api)
	return api.OpenAPI()
}
