package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/webapp"
)

func config() (webapp.Config, error) {
	cfg := webapp.Config{
		Addr:    envOr("LOYAL_WEB_ADDRESS", "127.0.0.1:8080"),
		AppHost: os.Getenv("LOYAL_WEB_APP_HOST"),
		Release: release(),
	}
	var err error
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

var stamp string

func release() string {
	if stamp != "" {
		return stamp
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var revision, modified string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}
	if revision == "" || modified == "true" {
		return "dirty"
	}
	return revision
}

func run(ctx context.Context) error {
	cfg, err := config()
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           webapp.New(cfg, webapp.Deps{}),
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
