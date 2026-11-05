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
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnstats"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/referral"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/webserve"
)

const WriteTimeout = max(earningsRefreshBudget, ensureSmartAccountBudget) + time.Minute

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
	WalletPush  *WalletPush
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
		goAPI.Use(servedByGo, onPaths(limitMobileEarn, earnMaxAPYPath), onPaths(limitMobileEarn, deskReadPaths...))
		registerAPI(humachi.New(goAPI, apiConfig()), cfg)
	})
	proxied := forward(cfg.Upstream)
	r.Handle(mobileEarnPrefix+"*", limitMobileEarn(proxied))
	r.Handle("/api/*", proxied)
	r.Handle("/ingest/*", newIngestProxy(mixpanelOrigin))
	r.Handle("/*", site(cfg))
	return r
}

func onPaths(wrap func(http.Handler) http.Handler, paths ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		wrapped := wrap(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if slices.Contains(paths, r.URL.Path) {
				wrapped.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
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
	if cfg.EarnMax != nil {
		newEarnMaxRoutes(nil, nil, *cfg.EarnMax, cfg.Release).registerPublicAPY(api)
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
	if cfg.WalletPush != nil && cfg.WalletPush.Secret != "" {
		registerWalletPush(api, cfg.WalletPush)
	}
	if cfg.Earn != nil {
		registerEarn(api, newEarnRoutes(cfg.Auth, cfg.AppDB, *cfg.Earn))
	}
	registerCSPReport(api, cfg.Earn)
	registerKaminoProxies(api)
	registerCherryLaunch(api)
	registerLogout(api)
	if cfg.CoinGeckoAPIKey != "" {
		details := newTokenDetails(cfg.CoinGeckoAPIKey)
		registerTokens(api, details)
		registerWebTokens(api, newWebTokens(details))
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
	translated := translatedPaths(pages)
	apex := apexHosts(cfg.AppHost)
	serve := func(w http.ResponseWriter, r *http.Request, marketing bool) {
		unknown := func() {
			if marketing && !appPath(path.Clean(r.URL.Path)) {
				app.NotFound(w, r)
				return
			}
			app.Shell(w, r, http.StatusNotFound)
		}
		switch pagePath := path.Clean(r.URL.Path); {
		case len(pages) == 0:
			http.Error(w, "web page manifest missing or invalid", http.StatusServiceUnavailable)
		case pagePath == "/500":
			if !app.Page(w, r, http.StatusInternalServerError) {
				app.Shell(w, r, http.StatusInternalServerError)
			}
		case pagePath == "/dev/txv1" && cfg.Environment == "prod":
			app.Shell(w, r, http.StatusNotFound)
		default:
			if slices.Contains(pages, pagePath) && app.Page(w, r, http.StatusOK) {
				return
			}
			if slices.Contains(pages, pagePath) || inviteLink(pagePath) {
				r = r.Clone(r.Context())
				r.URL.Path = "/"
			} else {
				info, err := fs.Stat(cfg.Dist, strings.TrimPrefix(pagePath, "/"))
				if err != nil || info.IsDir() || path.Base(pagePath) == "index.html" {
					unknown()
					return
				}
			}
			app.ServeHTTP(w, r)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slices.Contains(apex, requestHost(r)) && appPath(r.URL.Path) {
			vercelRedirect(w, "https://"+cfg.AppHost+r.URL.RequestURI(), http.StatusTemporaryRedirect)
			return
		}
		if trimmed := strings.TrimRight(r.URL.Path, "/"); trimmed != "" && trimmed != r.URL.Path {
			vercelRedirect(w, (&url.URL{Path: trimmed, RawQuery: r.URL.RawQuery}).String(), http.StatusPermanentRedirect)
			return
		}
		if cfg.AppHost != "" && requestHost(r) == cfg.AppHost {
			if r.URL.Path == "/" {
				http.Redirect(w, r, (&url.URL{Path: "/app", RawQuery: r.URL.RawQuery}).String(), http.StatusFound)
				return
			}
			serve(w, r, false)
			return
		}
		if !skippedByNextMatcher(r.URL.Path) && localeRedirect(w, r, translated) {
			return
		}
		serve(w, r, true)
	})
}

func apexHosts(appHost string) []string {
	apex, ok := strings.CutPrefix(appHost, "app.")
	if !ok {
		return nil
	}
	return []string{apex, "www." + apex}
}

func appPath(p string) bool {
	p = strings.ToLower(p)
	return p == "/app" || strings.HasPrefix(p, "/app/")
}

func redirect(w http.ResponseWriter, r *http.Request, target string, status int) {
	w.Header().Set("Location", target)
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(http.StatusText(status)))
	}
}

func vercelRedirect(w http.ResponseWriter, target string, status int) {
	w.Header().Set("Location", target)
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
	if status == http.StatusPermanentRedirect {
		w.Header().Set("Refresh", "0;url="+target)
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte("Redirecting...\n"))
}

func inviteLink(pagePath string) bool {
	code, ok := strings.CutPrefix(pagePath, "/i/")
	if !ok {
		return false
	}
	_, err := referral.ParseCode(code)
	return err == nil
}

var nextMatcherSkips = regexp.MustCompile(`^/(?:_next|api|favicon.ico|.*\..*)`)

func skippedByNextMatcher(path string) bool {
	return nextMatcherSkips.MatchString(path)
}

func requestHost(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.Host); err == nil {
		return strings.ToLower(host)
	}
	return strings.ToLower(r.Host)
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
	routes.registerPublicAPY(api)
	routes.registerRedeem(api)
	registerPush(api, nil, nil)
	registerWalletPush(api, nil)
	documentEarn(api)
	spec := api.OpenAPI()
	markNullableObjects(spec)
	return spec
}
