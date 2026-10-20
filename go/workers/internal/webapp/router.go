// Package webapp is loyal-web's HTTP surface: the embedded web app, the /api
// routes Go owns, and a forward of every other /api path to the Next
// deployment that still owns it.
package webapp

import (
	"net/http"
	"net/url"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/webserve"
)

type Config struct {
	Addr     string
	Upstream *url.URL // the Next deployment that answers /api paths Go does not
	PostHog  *url.URL // /ingest/* goes here
	AppHost  string   // requests for this host at / go to /app, as apps/web middleware.ts does
	Release  string
}

func New(cfg Config) http.Handler {
	r := chi.NewRouter()
	r.Use(headers(cfg))
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Get("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	api := humachi.New(r, apiConfig())
	RegisterAPI(api, Deps{})
	r.Handle("/api/*", forward(cfg.Upstream))
	r.Handle("/ingest/*", ingest(cfg.PostHog))

	app := webserve.Handler()
	r.Handle("/*", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if cfg.AppHost != "" && req.Host == cfg.AppHost && req.URL.Path == "/" {
			http.Redirect(w, req, "/app", http.StatusFound)
			return
		}
		app.ServeHTTP(w, req)
	}))
	return r
}

func apiConfig() huma.Config {
	config := huma.DefaultConfig("Loyal API", "1.0.0")
	config.DocsPath = ""
	config.OpenAPIPath = "/api/openapi"
	return config
}

// OpenAPI is the spec of every route RegisterAPI declares, for the TS client.
func OpenAPI() *huma.OpenAPI {
	api := humachi.New(chi.NewRouter(), apiConfig())
	RegisterAPI(api, Deps{})
	return api.OpenAPI()
}
