package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
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
	release, err := required("LOYAL_IMAGE_VERSION")
	if err != nil {
		return err
	}
	owner, err := engine.InstanceOwner(scope, instance, release)
	if err != nil {
		return err
	}
	switch scope {
	case "backyard":
		return runBackyard(ctx, owner, release)
	case "retail":
		return errors.New("retail transaction family integration incomplete")
	default:
		return errors.New("engine scope must be retail or backyard")
	}
}

func runBackyard(ctx context.Context, owner, release string) error {
	databaseURL, err := required("BACKYARD_DATABASE_URL")
	if err != nil {
		return err
	}
	rpcURL, err := required("BACKYARD_SOLANA_RPC_URL")
	if err != nil {
		return err
	}
	material, err := required("BACKYARD_POLICY_KEYPAIR")
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
	lane, err := backyard.NewEngine(backyard.EngineConfig{Database: database, RPC: rpc, Credentials: credentials, RouteKey: cfg.RouteKey, Config: backyard.DefaultConfig(), Owner: owner, Out: os.Stdout, ImageVersion: release})
	if err != nil {
		return err
	}
	return engine.Run(ctx, lane)
}

func main() {
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
		fmt.Fprintln(os.Stderr, "engine failed:", err)
		os.Exit(1)
	}
}
