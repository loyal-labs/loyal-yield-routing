package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/config"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/observability"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/worker"
)

func main() {
	logger := observability.Logger()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	cfg, err := config.FromEnv()
	if err != nil {
		logger.Error("invalid worker configuration", "event", "laserstream_worker_configuration_failed", "error", err)
		os.Exit(1)
	}
	shutdownOTEL, err := observability.InitOTEL(ctx)
	if err != nil {
		logger.Error("initialize OTEL", "error", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownOTEL(shutdownCtx)
	}()
	health := observability.NewHealth()
	metrics := observability.NewMetrics()
	server := &http.Server{Addr: cfg.HTTPAddress, Handler: health.Handler(cfg.ProgressTimeout), ReadHeaderTimeout: 5 * time.Second}
	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("health server listening", "address", cfg.HTTPAddress)
		serverErrors <- server.ListenAndServe()
	}()
	runtime, err := worker.New(ctx, cfg, logger, health, metrics)
	if err != nil {
		health.Fatal(err)
		logger.Error("initialize combined LaserStream worker", "event", "laserstream_worker_startup_failed", "error", err)
		shutdown(server, logger)
		os.Exit(1)
	}
	defer runtime.Close()
	runErrors := make(chan error, 1)
	go func() { runErrors <- runtime.Run(ctx) }()
	select {
	case err = <-runErrors:
		if err != nil {
			health.Fatal(err)
			logger.Error("combined LaserStream worker stopped", "event", "laserstream_worker_fatal", "recoveryRequired", true, "error", err)
		}
		cancel()
	case serverErr := <-serverErrors:
		if !errors.Is(serverErr, http.ErrServerClosed) {
			err = serverErr
			health.Fatal(err)
			logger.Error("health server stopped", "error", err)
		}
		cancel()
	case <-ctx.Done():
	}
	shutdown(server, logger)
	if err != nil {
		os.Exit(1)
	}
}
func shutdown(server *http.Server, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		logger.Error("shutdown health server", "error", err)
	}
}
