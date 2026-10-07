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
	metrics, err := engine.ListenMetrics(os.Getenv("LOYAL_METRICS_ADDRESS"), registry)
	if err != nil {
		return err
	}
	defer metrics.Close()
	runtime, err := worker.New(ctx, cfg, slog.Default(), facts)
	if err != nil {
		return err
	}
	defer runtime.Close()
	projector, err := runtime.NewATAProjector(ctx)
	if err != nil {
		return err
	}
	maintenance, err := runtime.NewMaintenance(ctx)
	if err != nil {
		return err
	}
	return engine.Run(ctx, runtime, projector, maintenance, metrics)
}

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
	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("observer failed", "error", err)
		os.Exit(1)
	}
}
