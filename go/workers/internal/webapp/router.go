package webapp

import (
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnstats"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/webserve"
)

type Config struct {
	Addr        string
	Upstream    *url.URL
	AppHost     string
	Release     string
	Ready       func() bool
	AppDB       *pgxpool.Pool
	Auth        *Auth
	EarnMax     *EarnMax
	PushDB      bool
	ContentDB   bool
	StatsDB     bool
	Dist        fs.FS
	Environment string

	CoinGeckoAPIKey string
}

func New(cfg Config) http.Handler {
	r := chi.NewRouter()
	r.Use(headers(cfg))
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Get("/readyz", ready(cfg.ready()))

	r.Group(func(goAPI chi.Router) {
		goAPI.Use(servedByGo)
		registerAPI(humachi.New(goAPI, apiConfig()), cfg)
	})
	proxied := forward(cfg.Upstream)
	r.Handle("/api/*", proxied)
	r.Handle("/ingest/*", proxied)
	r.Handle("/*", site(cfg))
	return r
}

func servedByGo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Loyal-API-Source", "go")
		next.ServeHTTP(w, r)
	})
}

func registerAPI(api huma.API, cfg Config) {
	if cfg.AppDB != nil {
		if cfg.StatsDB {
			earnstats.Register(api, cfg.AppDB)
		}
		if cfg.ContentDB {
			registerContent(api, cfg.AppDB)
		}
	}
	if cfg.Auth != nil && cfg.AppDB != nil {
		if cfg.EarnMax != nil {
			routes := newEarnMaxRoutes(cfg.Auth, cfg.AppDB, *cfg.EarnMax, cfg.Release)
			routes.registerReads(api)
			if len(cfg.EarnMax.Invites) > 0 {
				routes.registerRedeem(api)
			}
		}
		if cfg.PushDB {
			registerPush(api, cfg.Auth, cfg.AppDB)
		}
	}
	if cfg.CoinGeckoAPIKey != "" {
		registerTokens(api, newTokenDetails(cfg.CoinGeckoAPIKey))
	}
}

func (cfg Config) ready() func() bool {
	if cfg.Ready != nil {
		return cfg.Ready
	}
	return webserve.New(cfg.Dist).Ready
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
	app := webserve.New(cfg.Dist)
	serve := func(w http.ResponseWriter, r *http.Request) {
		switch pagePath := path.Clean(r.URL.Path); {
		case pagePath == "/500":
			app.Shell(w, r, http.StatusInternalServerError)
		case pagePath == "/dev/txv1" && cfg.Environment == "prod":
			app.Shell(w, r, http.StatusNotFound)
		default:
			app.ServeHTTP(w, r)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfg.AppHost != "" && requestHost(r) == cfg.AppHost {
			if r.URL.Path == "/" {
				http.Redirect(w, r, (&url.URL{Path: "/app", RawQuery: r.URL.RawQuery}).String(), http.StatusFound)
				return
			}
			serve(w, r)
			return
		}
		if !skippedByNextMatcher(r.URL.Path) && localeRedirect(w, r) {
			return
		}
		serve(w, r)
	})
}

var nextMatcherSkips = regexp.MustCompile(`^/(?:_next|api|favicon.ico|.*\..*)`)

func skippedByNextMatcher(path string) bool {
	return nextMatcherSkips.MatchString(path)
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
	config.CreateHooks = nil
	return config
}

func OpenAPI() *huma.OpenAPI {
	api := humachi.New(chi.NewRouter(), apiConfig())
	earnstats.Register(api, nil)
	registerContent(api, nil)
	registerTokens(api, nil)
	routes := newEarnMaxRoutes(nil, nil, EarnMax{}, "")
	routes.registerReads(api)
	routes.registerRedeem(api)
	registerPush(api, nil, nil)
	return api.OpenAPI()
}
