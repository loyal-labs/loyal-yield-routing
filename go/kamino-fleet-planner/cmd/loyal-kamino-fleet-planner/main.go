package main

import (
	"context"
	"errors"
	"log"
	"os/signal"
	"syscall"

	"github.com/loyal-labs/loyal-yield-routing/go/kamino-fleet-planner/internal/fleet"
)

func main() {
	config, err := fleet.ConfigFromEnvironment()
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	store, err := fleet.OpenStore(ctx, config.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	marketEvidence, err := fleet.OpenMarketEvidenceStore(ctx, config.TimescaleURL, config.TimescaleSchema)
	if err != nil {
		log.Fatal(err)
	}
	defer marketEvidence.Close()
	worker, err := fleet.NewWorker(config, store, fleet.NewRPCClient(config.RPCURL))
	if err != nil {
		log.Fatal(err)
	}
	if err := worker.SetMarketEvidence(marketEvidence); err != nil {
		log.Fatal(err)
	}
	if err := worker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
