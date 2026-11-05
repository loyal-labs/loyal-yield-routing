package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnstats"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/stats"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/webapp"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/webserve"
)

//go:embed all:dist
var embedded embed.FS

func open(ctx context.Context, name string) (*pgxpool.Pool, error) {
	dsn, err := engine.Credential(name)
	if err != nil {
		return nil, err
	}
	pool, err := db.OpenLazy(ctx, dsn, 4)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return pool, nil
}

var snapshotAppGrants = []webapp.Grant{
	webapp.TableGrant("public.app_users", "SELECT"),
	webapp.TableGrant("public.loyal_stats_snapshots", "SELECT"),
	webapp.TableGrant("public.loyal_stats_snapshots", "INSERT"),
	webapp.TableGrant("public.loyal_stats_snapshots", "UPDATE"),
}

var snapshotYieldGrants = []webapp.Grant{
	webapp.TableGrant("loyal_yield.user_yield_positions", "SELECT"),
	webapp.TableGrant("loyal_yield.managed_vaults", "SELECT"),
	webapp.TableGrant("loyal_yield.route_policies", "SELECT"),
	webapp.TableGrant("loyal_yield.vault_reserve_positions_current", "SELECT"),
	webapp.TableGrant("loyal_yield.vault_idle_token_balances_current", "SELECT"),
	webapp.TableGrant("loyal_yield.user_yield_position_holding_events", "SELECT"),
	webapp.TableGrant("loyal_yield.rebalance_decisions", "SELECT"),
	webapp.TableGrant("loyal_yield.balance_sweep_policies", "SELECT"),
	webapp.TableGrant("loyal_yield.balance_sweep_targets", "SELECT"),
}

func snapshotWriter(ctx context.Context, cfg stats.Config) (*earnstats.Refresher, error) {
	slackURL, err := engine.OptionalCredential("STATS_SLACK_WEBHOOK_URL")
	if err != nil {
		return nil, err
	}
	for _, c := range []struct {
		name   string
		pool   *pgxpool.Pool
		grants []webapp.Grant
	}{{"App", cfg.App, snapshotAppGrants}, {"Yield", cfg.Yield, snapshotYieldGrants}} {
		missing, err := webapp.MissingGrants(ctx, c.pool, c.grants)
		if err != nil || len(missing) > 0 {
			slog.Warn("stats snapshot writer disabled", "database", c.name, "missing", missing, "error", err)
			return nil, nil
		}
	}
	return earnstats.NewRefresher(cfg.App, cfg.Yield, slackURL), nil
}

func run(ctx context.Context) error {
	cfg := stats.Config{Addr: os.Getenv("LOYAL_STATS_ADDRESS"), Release: webserve.Release()}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:10082"
	}
	var err error
	if cfg.Dist, err = fs.Sub(embedded, "dist"); err != nil {
		return err
	}
	if cfg.App, err = open(ctx, "STATS_APP_DATABASE_URL"); err != nil {
		return err
	}
	defer cfg.App.Close()
	if cfg.Yield, err = open(ctx, "STATS_YIELD_DATABASE_URL"); err != nil {
		return err
	}
	defer cfg.Yield.Close()
	writer, err := snapshotWriter(ctx, cfg)
	if err != nil {
		return err
	}
	var writing sync.WaitGroup
	defer writing.Wait()
	writerCtx, stopWriter := context.WithCancel(ctx)
	defer stopWriter()
	if writer != nil {
		writing.Go(func() { writer.Run(writerCtx) })
	}
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           stats.New(cfg),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	failed := make(chan error, 1)
	go func() { failed <- server.ListenAndServe() }()
	slog.Info("loyal-stats listening", "address", cfg.Addr, "release", cfg.Release)
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
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: loyal-stats")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("loyal-stats stopped", "error", err)
		os.Exit(1)
	}
}
