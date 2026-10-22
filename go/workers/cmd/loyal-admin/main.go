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
	"path/filepath"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/admin"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/webserve"
)

//go:embed all:dist
var embedded embed.FS

func optional(name string) (string, error) {
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if dir == "" {
		return "", nil
	}
	if _, err := os.Stat(filepath.Join(dir, name)); errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("cannot inspect credential %s", name)
	}
	return engine.Credential(name)
}

func openOptional(ctx context.Context, name string, maxConnections int32) (*pgxpool.Pool, error) {
	dsn, err := optional(name)
	if err != nil || dsn == "" {
		return nil, err
	}
	pool, err := db.Open(ctx, dsn, maxConnections)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return pool, nil
}

func config(ctx context.Context) (admin.Config, func(), error) {
	cfg := admin.Config{Addr: os.Getenv("LOYAL_ADMIN_ADDRESS"), Release: webserve.Release(), Deps: &admin.Deps{}}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:8081"
	}
	closers := []func(){}
	closeAll := func() {
		for _, close := range closers {
			close()
		}
	}
	var err error
	if cfg.Dist, err = fs.Sub(embedded, "dist"); err != nil {
		return cfg, closeAll, err
	}
	if cfg.Auth.User, err = engine.Credential("ADMIN_USER"); err != nil {
		return cfg, closeAll, err
	}
	if cfg.Auth.Password, err = engine.Credential("ADMIN_PASSWORD"); err != nil {
		return cfg, closeAll, err
	}
	appDSN, err := engine.Credential("ADMIN_APP_DATABASE_URL")
	if err != nil {
		return cfg, closeAll, err
	}
	if cfg.Deps.App, err = db.Open(ctx, appDSN, 4); err != nil {
		return cfg, closeAll, fmt.Errorf("ADMIN_APP_DATABASE_URL: %w", err)
	}
	closers = append(closers, cfg.Deps.App.Close)
	for _, pool := range []struct {
		name string
		into **pgxpool.Pool
	}{{"ADMIN_YIELD_DATABASE_URL", &cfg.Deps.Yield}, {"ADMIN_TIMESCALE_DATABASE_URL", &cfg.Deps.Timescale}} {
		if *pool.into, err = openOptional(ctx, pool.name, 4); err != nil {
			return cfg, closeAll, err
		}
		if *pool.into != nil {
			closers = append(closers, (*pool.into).Close)
		} else {
			slog.Warn("loyal-admin family disabled", "credential", pool.name)
		}
	}
	for _, value := range []struct {
		name string
		into *string
	}{
		{"LIBRARY_UPLOAD_TOKEN", &cfg.Deps.LibraryUploadToken},
		{"CLICKSTACK_READ_KEY", &cfg.Deps.ClickStackReadKey},
		{"EXPO_ACCESS_TOKEN", &cfg.Deps.ExpoAccessToken},
	} {
		if *value.into, err = optional(value.name); err != nil {
			return cfg, closeAll, err
		}
		if *value.into == "" {
			slog.Warn("loyal-admin family disabled", "credential", value.name)
		}
	}
	if base := os.Getenv("LOYAL_ADMIN_APP_API_BASE_URL"); base != "" {
		parsed, err := url.Parse(base)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return cfg, closeAll, errors.New("LOYAL_ADMIN_APP_API_BASE_URL: want an origin like https://host")
		}
		cfg.Deps.AppAPIBaseURL = parsed
	}
	return cfg, closeAll, nil
}

func run(ctx context.Context) error {
	cfg, closeAll, err := config(ctx)
	defer closeAll()
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           admin.New(cfg),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	failed := make(chan error, 1)
	go func() { failed <- server.ListenAndServe() }()
	slog.Info("loyal-admin listening", "address", cfg.Addr, "release", cfg.Release)
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
		if err := json.NewEncoder(os.Stdout).Encode(admin.OpenAPI()); err != nil {
			slog.Error("loyal-admin openapi", "error", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: loyal-admin [openapi]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("loyal-admin stopped", "error", err)
		os.Exit(1)
	}
}
