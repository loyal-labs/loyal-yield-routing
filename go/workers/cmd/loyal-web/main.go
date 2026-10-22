package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/webapp"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/webserve"
)

//go:embed all:dist
var embedded embed.FS

var appEnvironment = "prod"

func config() (webapp.Config, error) {
	cfg := webapp.Config{
		Addr:        envOr("LOYAL_WEB_ADDRESS", "127.0.0.1:8080"),
		AppHost:     os.Getenv("LOYAL_WEB_APP_HOST"),
		Release:     webserve.Release(),
		Environment: appEnvironment,
	}
	var err error
	if cfg.Dist, err = fs.Sub(embedded, "dist"); err != nil {
		return cfg, err
	}
	if cfg.Upstream, err = parseOrigin("LOYAL_WEB_UPSTREAM", "https://app.askloyal.com"); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func parseOrigin(name, fallback string) (*url.URL, error) {
	origin, err := url.Parse(envOr(name, fallback))
	if err != nil || origin.Scheme == "" || origin.Host == "" {
		return nil, fmt.Errorf("%s: want an origin like https://host", name)
	}
	return origin, nil
}

func degraded(routes string, err error) {
	slog.Warn(routes+" not served by Go", "reason", err.Error())
}

func optionalCredential(routes, name string) (string, bool) {
	value, err := engine.OptionalCredential(name)
	if err != nil {
		degraded(routes, err)
		return "", false
	}
	return value, true
}

func openDB(ctx context.Context, routes, credential string) *pgxpool.Pool {
	dsn, ok := optionalCredential(routes, credential)
	if !ok || dsn == "" {
		return nil
	}
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		degraded(routes, fmt.Errorf("invalid %s credential", credential))
		return nil
	}
	poolConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		degraded(routes, fmt.Errorf("cannot open the %s pool", credential))
		return nil
	}
	return pool
}

func openAuth(appDB *pgxpool.Pool, sessionGranted bool) (*webapp.Auth, *chain.Client) {
	auth, rpcClient, err := buildAuth(appDB, sessionGranted)
	if err != nil {
		degraded("session routes", err)
		return nil, nil
	}
	return auth, rpcClient
}

func buildAuth(appDB *pgxpool.Pool, sessionGranted bool) (*webapp.Auth, *chain.Client, error) {
	secret, err := engine.OptionalCredential("AUTH_JWT_SECRET")
	if err != nil {
		return nil, nil, err
	}
	publicKey, err := engine.OptionalCredential("AUTH_JWT_RS256_PUBLIC_KEY")
	if err != nil {
		return nil, nil, err
	}
	if secret == "" && publicKey == "" {
		return nil, nil, nil
	}
	endpoint, err := engine.OptionalCredential("WEB_SOLANA_RPC_URL")
	if err != nil {
		return nil, nil, err
	}
	if appDB == nil || endpoint == "" {
		return nil, nil, errors.New("needs WEB_APP_DATABASE_URL and WEB_SOLANA_RPC_URL")
	}
	if !sessionGranted {
		return nil, nil, errors.New("web app role lacks the session grants")
	}
	rpcClient, err := chain.New(endpoint, 10*time.Second)
	if err != nil {
		return nil, nil, errors.New("WEB_SOLANA_RPC_URL is not a usable RPC URL")
	}
	auth, err := webapp.NewAuth(secret, publicKey, appDB, rpcClient)
	return auth, rpcClient, err
}

func granted(ctx context.Context, pool *pgxpool.Pool, routes string, grants []webapp.Grant) bool {
	missing, err := webapp.MissingGrants(ctx, pool, grants)
	switch {
	case err != nil:
		slog.Warn(routes+" not served by Go: grant check failed", "error", err)
	case len(missing) > 0:
		slog.Warn(routes+" not served by Go: role lacks grants", "missing", missing)
	default:
		return true
	}
	return false
}

type grantVerdicts struct{ Content, Stats, Session, EarnMax, Push, Yield bool }

func checkGrants(ctx context.Context, appDB, yieldDB *pgxpool.Pool) grantVerdicts {
	ctx, cancel := context.WithTimeout(ctx, grantBound)
	defer cancel()
	var v grantVerdicts
	var wg sync.WaitGroup
	for _, c := range []struct {
		ok     *bool
		pool   *pgxpool.Pool
		routes string
		grants []webapp.Grant
	}{
		{&v.Content, appDB, "content reads", webapp.ContentGrants},
		{&v.Stats, appDB, "earn stats", webapp.StatsGrants},
		{&v.Session, appDB, "session routes", webapp.AuthGrants},
		{&v.EarnMax, appDB, "Earn MAX routes", webapp.EarnMaxGrants},
		{&v.Push, appDB, "push tokens", webapp.PushGrants},
		{&v.Yield, yieldDB, "Earn MAX route state", webapp.YieldGrants},
	} {
		if c.pool != nil {
			wg.Go(func() { *c.ok = granted(ctx, c.pool, c.routes, c.grants) })
		}
	}
	wg.Wait()
	return v
}

func earnMax(rpcClient *chain.Client, grants grantVerdicts, yieldDB *pgxpool.Pool) *webapp.EarnMax {
	const routes = "Earn MAX routes"
	cluster, err := webapp.ParseSolanaCluster(os.Getenv("LOYAL_WEB_SOLANA_CLUSTER"))
	if err != nil {
		degraded(routes, err)
		return nil
	}
	if !grants.EarnMax {
		return nil
	}
	var invites webapp.InviteCodes
	if hashes, ok := optionalCredential("Earn MAX invite redemption", "EARN_MAX_INVITE_CODE_HASHES"); ok {
		if invites, err = webapp.ParseInviteCodes(hashes); err != nil {
			degraded("Earn MAX invite redemption", err)
		}
	}
	var yield earnmax.Querier
	if yieldDB != nil && grants.Yield {
		yield = yieldDB
	}
	return &webapp.EarnMax{Reader: earnmax.NewReader(rpcClient, yield), Cluster: cluster, Invites: invites}
}

func run(ctx context.Context) error {
	cfg, err := config()
	if err != nil {
		return err
	}
	cfg.CoinGeckoAPIKey, _ = optionalCredential("token detail routes", "COINGECKO_API_KEY")
	appDB := openDB(ctx, "app database routes", "WEB_APP_DATABASE_URL")
	if appDB != nil {
		defer appDB.Close()
	}
	yieldDB := openDB(ctx, "Earn MAX APY", "WEB_YIELD_DATABASE_URL")
	if yieldDB != nil {
		defer yieldDB.Close()
	}
	grants := checkGrants(ctx, appDB, yieldDB)
	if appDB != nil {
		cfg.AppDB = appDB
		cfg.ContentDB = grants.Content
		cfg.StatsDB = grants.Stats
	}
	if auth, rpcClient := openAuth(appDB, grants.Session); auth != nil {
		cfg.Auth = auth
		cfg.EarnMax = earnMax(rpcClient, grants, yieldDB)
		cfg.PushDB = grants.Push
	}
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           webapp.New(cfg),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	failed := make(chan error, 1)
	go func() { failed <- server.ListenAndServe() }()
	slog.Info("loyal-web listening", "address", cfg.Addr, "release", cfg.Release, "upstream", cfg.Upstream.String())
	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-failed; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if len(os.Args) == 2 && os.Args[1] == "openapi" {
		if err := json.NewEncoder(os.Stdout).Encode(webapp.OpenAPI()); err != nil {
			slog.Error("loyal-web openapi", "error", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: loyal-web [openapi]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("loyal-web stopped", "error", err)
		os.Exit(1)
	}
}

var grantBound = 8 * time.Second
