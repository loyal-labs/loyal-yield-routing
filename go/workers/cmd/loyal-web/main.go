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

func openDB(ctx context.Context, routes, credential string) (pool *pgxpool.Pool, ok bool) {
	dsn, ok := optionalCredential(routes, credential)
	if !ok || dsn == "" {
		return nil, ok
	}
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		degraded(routes, fmt.Errorf("invalid %s credential", credential))
		return nil, false
	}
	poolConfig.MaxConns = 4
	pool, err = pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		degraded(routes, fmt.Errorf("cannot open the %s pool", credential))
		return nil, false
	}
	return pool, true
}

func openAuth(ctx context.Context, appDB *pgxpool.Pool) (*webapp.Auth, *chain.Client) {
	auth, rpcClient, err := buildAuth(ctx, appDB)
	if err != nil {
		degraded("session routes", err)
		return nil, nil
	}
	return auth, rpcClient
}

func buildAuth(ctx context.Context, appDB *pgxpool.Pool) (*webapp.Auth, *chain.Client, error) {
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
	if !granted(ctx, appDB, "session routes", webapp.AuthGrants) {
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

func earnMax(ctx context.Context, appDB *pgxpool.Pool, rpcClient *chain.Client) (*webapp.EarnMax, *pgxpool.Pool) {
	const routes = "Earn MAX routes"
	cluster, err := webapp.ParseSolanaCluster(os.Getenv("LOYAL_WEB_SOLANA_CLUSTER"))
	if err != nil {
		degraded(routes, err)
		return nil, nil
	}
	if !granted(ctx, appDB, routes, webapp.EarnMaxGrants) {
		return nil, nil
	}
	var invites webapp.InviteCodes
	if hashes, ok := optionalCredential("Earn MAX invite redemption", "EARN_MAX_INVITE_CODE_HASHES"); ok {
		if invites, err = webapp.ParseInviteCodes(hashes); err != nil {
			degraded("Earn MAX invite redemption", err)
		}
	}
	yieldDB, ok := openDB(ctx, routes, "WEB_YIELD_DATABASE_URL")
	if !ok {
		return nil, nil
	}
	var yield earnmax.Querier
	if yieldDB != nil && granted(ctx, yieldDB, "Earn MAX route state", webapp.YieldGrants) {
		yield = yieldDB
	}
	return &webapp.EarnMax{Reader: earnmax.NewReader(rpcClient, yield), Cluster: cluster, Invites: invites}, yieldDB
}

func run(ctx context.Context) error {
	cfg, err := config()
	if err != nil {
		return err
	}
	cfg.CoinGeckoAPIKey, _ = optionalCredential("token detail routes", "COINGECKO_API_KEY")
	appDB, _ := openDB(ctx, "app database routes", "WEB_APP_DATABASE_URL")
	if appDB != nil {
		defer appDB.Close()
		cfg.AppDB = appDB
		cfg.ContentDB = granted(ctx, appDB, "content reads", webapp.ContentGrants)
	}
	auth, rpcClient := openAuth(ctx, appDB)
	if auth != nil {
		var yieldDB *pgxpool.Pool
		if cfg.EarnMax, yieldDB = earnMax(ctx, appDB, rpcClient); yieldDB != nil {
			defer yieldDB.Close()
		}
		cfg.Auth = auth
		cfg.PushDB = granted(ctx, appDB, "push tokens", webapp.PushGrants)
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
