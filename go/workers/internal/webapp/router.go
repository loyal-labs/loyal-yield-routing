package webapp

import (
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

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
	Earn        *Earn
	PushDB      bool
	ContentDB   bool
	StatsDB     bool
	Dist        fs.FS
	Environment string

	CoinGeckoAPIKey string
}

func New(cfg Config) http.Handler {
	r := chi.NewRouter()
	r.Use(headers(cfg), redirectHTTPS)
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Get("/readyz", ready(cfg.ready()))

	r.Group(func(goAPI chi.Router) {
		goAPI.Use(servedByGo)
		registerAPI(humachi.New(goAPI, apiConfig()), cfg)
	})
	proxied := forward(cfg.Upstream)
	r.Handle(mobileEarnPrefix+"*", limitMobileEarn(proxied))
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
	if cfg.Earn != nil {
		registerEarn(api, newEarnRoutes(cfg.Auth, cfg.AppDB, *cfg.Earn))
	}
	if cfg.CoinGeckoAPIKey != "" {
		registerTokens(api, newTokenDetails(cfg.CoinGeckoAPIKey))
	}
}

func (cfg Config) ready() func() bool {
	if cfg.Ready != nil {
		return cfg.Ready
	}
	pages := pagePaths(cfg.Dist)
	return func() bool { return len(pages) > 0 && webserve.New(cfg.Dist).Ready() }
}

func pagePaths(dist fs.FS) []string {
	if dist == nil {
		return nil
	}
	body, err := fs.ReadFile(dist, "page-paths.json")
	if err != nil {
		return nil
	}
	var pages []string
	if json.Unmarshal(body, &pages) != nil {
		return nil
	}
	return pages
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
	pages := pagePaths(cfg.Dist)
	serve := func(w http.ResponseWriter, r *http.Request) {
		switch pagePath := path.Clean(r.URL.Path); {
		case len(pages) == 0:
			http.Error(w, "web page manifest missing or invalid", http.StatusServiceUnavailable)
		case pagePath == "/500":
			app.Shell(w, r, http.StatusInternalServerError)
		case pagePath == "/dev/txv1" && cfg.Environment == "prod":
			app.Shell(w, r, http.StatusNotFound)
		default:
			if slices.Contains(pages, pagePath) {
				r = r.Clone(r.Context())
				r.URL.Path = "/"
			} else {
				info, err := fs.Stat(cfg.Dist, strings.TrimPrefix(pagePath, "/"))
				if err != nil || info.IsDir() {
					app.Shell(w, r, http.StatusNotFound)
					return
				}
			}
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
	documentEarn(api)
	spec := api.OpenAPI()
	markNullableObjects(spec)
	return spec
}
