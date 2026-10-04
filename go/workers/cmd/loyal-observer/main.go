package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/config"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/observability"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/worker"
)

func run(ctx context.Context) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	health := observability.NewHealth()
	health.SetDomainReady("earn", false)
	metrics := observability.NewMetrics()
	shutdown, err := observability.InitOTEL(ctx)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(cleanup)
	}()
	runtime, err := worker.New(ctx, cfg, observability.Logger(), health, metrics)
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
	server, err := engine.ListenHTTP(cfg.HTTPAddress, health.Handler(cfg.ProgressTimeout))
	if err != nil {
		return err
	}
	defer server.Close()
	return engine.Run(ctx, runtime, projector, maintenance, server)
}

func main() {
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
		fmt.Fprintln(os.Stderr, "observer failed:", err)
		os.Exit(1)
	}
}
