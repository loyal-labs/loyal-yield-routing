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
	"strings"
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

const minConnections = "2"

func poolDSN(dsn string, extra map[string]string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Host == "" {
		return "", errors.New("want a database URL")
	}
	query := parsed.Query()
	query.Set("pool_min_conns", minConnections)
	for key, value := range extra {
		query.Set(key, value)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func openOptional(ctx context.Context, name string, maxConnections int32, extra map[string]string) (*pgxpool.Pool, error) {
	dsn, err := optional(name)
	if err != nil || dsn == "" {
		return nil, err
	}
	if dsn, err = poolDSN(dsn, extra); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	pool, err := db.Open(ctx, dsn, maxConnections)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return pool, nil
}

func httpsURL(name string, allowPath bool) (*url.URL, error) {
	parsed, err := url.Parse(os.Getenv(name))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(!allowPath && strings.Trim(parsed.Path, "/") != "") {
		return nil, fmt.Errorf("%s: want an https URL with a host", name)
	}
	return parsed, nil
}

func r2Config() (*admin.R2Config, error) {
	id, err := optional("R2_ACCESS_KEY_ID")
	if err != nil {
		return nil, err
	}
	secret, err := optional("R2_SECRET_ACCESS_KEY")
	if err != nil {
		return nil, err
	}
	bucket := strings.TrimSpace(os.Getenv("LOYAL_ADMIN_R2_BUCKET"))
	if id == "" || secret == "" || bucket == "" || os.Getenv("LOYAL_ADMIN_R2_ENDPOINT") == "" || os.Getenv("LOYAL_ADMIN_CDN_BASE_URL") == "" {
		return nil, nil
	}
	cfg := &admin.R2Config{AccessKeyID: id, SecretAccessKey: secret, Bucket: bucket}
	if strings.Contains(bucket, "/") {
		return nil, errors.New("LOYAL_ADMIN_R2_BUCKET: want a bucket name")
	}
	if cfg.Endpoint, err = httpsURL("LOYAL_ADMIN_R2_ENDPOINT", false); err != nil {
		return nil, err
	}
	if cfg.CDNBase, err = httpsURL("LOYAL_ADMIN_CDN_BASE_URL", true); err != nil {
		return nil, err
	}
	var prefix []string
	for _, segment := range strings.Split(os.Getenv("LOYAL_ADMIN_R2_PREFIX"), "/") {
		switch segment = strings.TrimSpace(segment); segment {
		case "":
		case ".", "..":
			return nil, errors.New("LOYAL_ADMIN_R2_PREFIX: contains an invalid path segment")
		default:
			prefix = append(prefix, segment)
		}
	}
	cfg.Prefix = strings.Join(prefix, "/")
	return cfg, nil
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
	if appDSN, err = poolDSN(appDSN, nil); err != nil {
		return cfg, closeAll, fmt.Errorf("ADMIN_APP_DATABASE_URL: %w", err)
	}
	if cfg.Deps.App, err = db.Open(ctx, appDSN, 4); err != nil {
		return cfg, closeAll, fmt.Errorf("ADMIN_APP_DATABASE_URL: %w", err)
	}
	closers = append(closers, cfg.Deps.App.Close)
	for _, pool := range []struct {
		name  string
		into  **pgxpool.Pool
		extra map[string]string
	}{
		{"ADMIN_YIELD_DATABASE_URL", &cfg.Deps.Yield, map[string]string{"options": admin.SessionTimeoutOptions}},
		{"ADMIN_TIMESCALE_DATABASE_URL", &cfg.Deps.Timescale, nil},
	} {
		if *pool.into, err = openOptional(ctx, pool.name, 8, pool.extra); err != nil {
			return cfg, closeAll, err
		}
		if *pool.into != nil {
			closers = append(closers, (*pool.into).Close)
			if pool.extra != nil {
				if err = admin.CheckSessionTimeout(ctx, *pool.into); err != nil {
					return cfg, closeAll, fmt.Errorf("%s: %w", pool.name, err)
				}
			}
		} else {
			slog.Warn("loyal-admin family disabled", "credential", pool.name)
		}
	}
	if cfg.Deps.ClickStackReadKey, err = optional("CLICKSTACK_READ_KEY"); err != nil {
		return cfg, closeAll, err
	}
	if cfg.Deps.ClickStackReadKey == "" {
		slog.Warn("loyal-admin family disabled", "credential", "CLICKSTACK_READ_KEY")
	}
	if cfg.Deps.R2, err = r2Config(); err != nil {
		return cfg, closeAll, err
	}
	if cfg.Deps.R2 == nil {
		slog.Warn("loyal-admin family disabled", "family", "library upload")
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
