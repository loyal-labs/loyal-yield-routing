package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
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
	families := []engine.Family{engine.FamilyBackyard}
	if scope == "retail" {
		families = []engine.Family{engine.FamilyAutodeposit, engine.FamilyFleet, engine.FamilyMultiply, engine.FamilyLookup}
	}
	// Each owned family's staleness clock starts when the process starts.
	for _, family := range families {
		facts.Progress(family)
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

func runBackyard(ctx context.Context, owner string, facts *engine.Facts, metrics engine.Lane) error {
	databaseURL, err := engine.Credential("BACKYARD_DATABASE_URL")
	if err != nil {
		return err
	}
	rpcURL, err := engine.Credential("BACKYARD_SOLANA_RPC_URL")
	if err != nil {
		return err
	}
	material, err := engine.Credential("BACKYARD_POLICY_KEYPAIR")
	if err != nil {
		return err
	}
	credentials, err := backyard.ParseCredentials(material)
	if err != nil {
		return err
	}
	cfg := backyard.RuntimeConfig{DatabaseURL: databaseURL, RPCURL: rpcURL, RouteKey: backyard.FixedRouteKey}
	if err := cfg.Validate(); err != nil {
		return err
	}
	database, err := backyard.OpenDatabase(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer database.Close()
	rpc, err := backyard.NewRPCClient(rpcURL)
	if err != nil {
		return err
	}
	lane, err := backyard.NewEngine(backyard.EngineConfig{Database: database, RPC: rpc, Credentials: credentials, RouteKey: cfg.RouteKey, Config: backyard.DefaultConfig(), Owner: owner, Out: os.Stdout, ImageVersion: engine.Release})
	if err != nil {
		return err
	}
	return engine.Run(ctx, lane, metrics)
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if len(os.Args) == 2 && os.Args[1] == "--role-probe" {
		fmt.Println(`{"schemaVersion":1,"role":"engine","networkAccessed":false,"secretsLoaded":false,"databaseMutated":false,"transactionSent":false}`)
		return
	}
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: loyal-engine [--role-probe]")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("engine failed", "error", err)
		os.Exit(1)
	}
}
