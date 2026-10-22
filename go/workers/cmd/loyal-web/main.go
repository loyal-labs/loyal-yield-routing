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

func openAppDB(ctx context.Context) (*pgxpool.Pool, error) {
	dsn, err := engine.OptionalCredential("WEB_APP_DATABASE_URL")
	if err != nil || dsn == "" {
		return nil, err
	}
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid web app database credential")
	}
	poolConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, errors.New("cannot open web app database pool")
	}
	return pool, nil
}

func openAuth(appDB *pgxpool.Pool) (*webapp.Auth, error) {
	secret, err := engine.OptionalCredential("AUTH_JWT_SECRET")
	if err != nil {
		return nil, err
	}
	publicKey, err := engine.OptionalCredential("AUTH_JWT_RS256_PUBLIC_KEY")
	if err != nil {
		return nil, err
	}
	if secret == "" && publicKey == "" {
		return nil, nil
	}
	endpoint, err := engine.OptionalCredential("WEB_SOLANA_RPC_URL")
	if err != nil {
		return nil, err
	}
	var rpcClient *chain.Client
	if endpoint != "" {
		if rpcClient, err = chain.New(endpoint, 10*time.Second); err != nil {
			return nil, err
		}
	}
	return webapp.NewAuth(secret, publicKey, appDB, rpcClient)
}

func run(ctx context.Context) error {
	cfg, err := config()
	if err != nil {
		return err
	}
	appDB, err := openAppDB(ctx)
	if err != nil {
		return err
	}
	if appDB != nil {
		defer appDB.Close()
		cfg.AppDB = appDB
	}
	if cfg.Auth, err = openAuth(appDB); err != nil {
		return err
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
