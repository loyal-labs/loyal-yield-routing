package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/config"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/worker"
	"github.com/prometheus/client_golang/prometheus"
)

func run(ctx context.Context) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	registry := prometheus.NewRegistry()
	facts := engine.NewFacts(registry)
	facts.Own(engine.FamilyObserver)
	metrics, err := engine.ListenMetrics(os.Getenv("LOYAL_METRICS_ADDRESS"), registry)
	if err != nil {
		return err
	}
	defer metrics.Close()
	lost, err := engine.HoldFamily(ctx, cfg.NeonDatabaseURL, engine.FamilyObserver)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		select {
		case <-lost:
			cancel(errLockLost)
		case <-ctx.Done():
		}
	}()
	runtime, err := worker.New(ctx, cfg, slog.Default(), facts)
	if err != nil {
		return err
	}
	defer runtime.Close()
	projector, err := runtime.NewATAProjector(ctx)
	if err != nil {
		return err
	}
	lanes := []engine.Lane{runtime, projector, metrics}
	maintenance, err := runtime.NewMaintenance(ctx)
	if err != nil {
		return err
	}
	if maintenance != nil {
		lanes = append(lanes, maintenance)
	}
	err = engine.Run(ctx, lanes...)
	if cause := context.Cause(ctx); errors.Is(cause, errLockLost) {
		return cause
	}
	return err
}

var errLockLost = errors.New("observer family lock lost")

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if len(os.Args) == 2 && os.Args[1] == "--role-probe" {
		fmt.Println(`{"schemaVersion":1,"role":"observer","networkAccessed":false,"secretsLoaded":false,"databaseMutated":false,"transactionSent":false}`)
		return
	}
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: loyal-observer [--role-probe]")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	slog.Info("observer_start", "release", engine.Release)
	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("observer failed", "error", err)
		os.Exit(1)
	}
}
