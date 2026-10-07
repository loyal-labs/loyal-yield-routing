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
	"github.com/prometheus/client_golang/prometheus"
)

func required(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return "", fmt.Errorf("required configuration missing: %s", name)
	}
	return value, nil
}

func run(ctx context.Context) error {
	scope, err := required("LOYAL_WORKER_SCOPE")
	if err != nil {
		return err
	}
	instance, err := required("LOYAL_WORKER_INSTANCE")
	if err != nil {
		return err
	}
	owner, err := engine.InstanceOwner(scope, instance, engine.Release)
	if err != nil {
		return err
	}
	if scope != "backyard" && scope != "retail" {
		return errors.New("engine scope must be retail or backyard")
	}
	registry := prometheus.NewRegistry()
	facts := engine.NewFacts(registry)
	if scope == "retail" {
		facts.Own(engine.FamilyAutodeposit, engine.FamilyFleet, engine.FamilyMultiply, engine.FamilyLookup)
	} else {
		facts.Own(engine.FamilyBackyard)
	}
	metrics, err := engine.ListenMetrics(os.Getenv("LOYAL_METRICS_ADDRESS"), registry)
	if err != nil {
		return err
	}
	defer metrics.Close()
	if scope == "backyard" {
		return runBackyard(ctx, owner, facts, metrics)
	}
	return runRetail(ctx, owner, facts, metrics)
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if len(os.Args) == 2 && os.Args[1] == "--role-probe" {
		fmt.Println(`{"schemaVersion":1,"role":"engine","networkAccessed":false,"secretsLoaded":false,"databaseMutated":false,"transactionSent":false}`)
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if len(os.Args) > 1 && os.Args[1] == "backyard" {
		if err := runBackyardOperator(ctx, os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "backyard operator command failed:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: loyal-engine [--role-probe | backyard <operator command>]")
		os.Exit(2)
	}
	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("engine failed", "error", err)
		os.Exit(1)
	}
}
