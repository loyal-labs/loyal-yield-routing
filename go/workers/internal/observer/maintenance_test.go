package observer

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"log/slog"
	"testing"
	"time"
)

func offlineClosedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig("postgresql://workers_v2@127.0.0.1:1/workers_v2_observer")
	if err != nil {
		t.Fatal(err)
	}
	config.MinConns = 0
	config.BeforeConnect = func(context.Context, *pgx.ConnConfig) error { return errors.New("offline test prohibits dialing") }
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	return pool
}
func TestMaintenanceOutageRetriesReportsEachFailureAndCancellationJoins(t *testing.T) {
	pool := offlineClosedPool(t)
	failures := make(chan struct{}, 16)
	m, err := NewMaintenance(pool, pool, MaintenanceConfig{Cluster: "mainnet-beta", MediumMarkets: []string{benchmarkMarket}, PriceRPC: &priceFixtureRPC{}, HealthObservation: time.Millisecond, RetryInterval: 2 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), OnError: func() { failures <- struct{}{} }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	// The first failed pass and actual retries are each reported.
	for i := 0; i < 3; i++ {
		select {
		case <-failures:
		case <-ctx.Done():
			t.Fatal("ordinary outage stopped retries")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run ended without caller cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("maintenance did not join cancelled caller")
	}
}
func TestFixedMainnetCatalogCannotBeLabelledDevnet(t *testing.T) {
	pool := offlineClosedPool(t)
	_, err := NewMaintenance(pool, pool, MaintenanceConfig{Cluster: "devnet", MediumMarkets: []string{benchmarkMarket}, PriceRPC: &priceFixtureRPC{}, OnError: func() {}})
	if err == nil {
		t.Fatal("mainnet product universe accepted a devnet namespace")
	}
}

func TestChangedCustodyNamespaceBlocksAllProductWrites(t *testing.T) {
	pool := offlineClosedPool(t)
	foreign := errors.New("foreign active custody")
	m, err := NewMaintenance(pool, pool, MaintenanceConfig{Cluster: "mainnet-beta", MediumMarkets: []string{benchmarkMarket}, PriceRPC: &priceFixtureRPC{}, OnError: func() {}, ValidateNamespace: func(context.Context) error { return foreign }})
	if err != nil {
		t.Fatal(err)
	}
	report, err := m.Tick(context.Background(), time.Now().UTC())
	if !errors.Is(err, foreign) || report.HealthPublished || report.Allocation != nil || report.ModelPublished || report.SharePrices != 0 {
		t.Fatalf("namespace failure reached product publication: report=%+v err=%v", report, err)
	}
}
