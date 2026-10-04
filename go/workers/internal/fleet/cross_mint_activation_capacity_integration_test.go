package fleet

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func crossMintCapacityFixtureStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("FLEET_TEST_CROSS_MINT_CAPTURE_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires root-registered disposable cross-mint capture database")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme != "postgresql" || u.Hostname() != "127.0.0.1" || !registeredFleetFixturePort(u.Port()) || u.Path != "/fleet_cross_mint_capture" || u.User == nil || u.User.Username() != "workers_v2" || u.Fragment != "" {
		t.Fatal("requires registered loopback workers_v2 /fleet_cross_mint_capture before connecting")
	}
	if _, present := u.User.Password(); present {
		t.Fatal("disposable fixture cannot contain credentials")
	}
	for key, values := range u.Query() {
		if key != "sslmode" || len(values) != 1 || values[0] != "disable" {
			t.Fatal("unregistered database connection option")
		}
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 2
	config.ConnConfig.RuntimeParams["statement_timeout"] = "10000"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = pool.Ping(ctx); err != nil {
		t.Fatal("configured registered capture database unavailable:", err)
	}
	store, err := NewStoreFromPool(pool)
	if err != nil {
		t.Fatal(err)
	}
	return store, ctx
}

// These tests execute the same concrete private telemetry capture used after
// the completed source producer. Supplied observations are synthetic SQL test
// inputs: these tests do not certify policies, simulation or chain freshness.
func TestCrossMintActivationCapacityCapturesActualRegisteredFrontier(t *testing.T) {
	store, ctx := crossMintCapacityFixtureStore(t)
	out, lease := crossMintCapacityObservationFixture(time.Now())
	lease.Cluster = fmt.Sprintf("capture-%d", time.Now().UnixNano())
	p, err := store.captureCrossMintActivationCapacity(ctx, out, lease)
	if err != nil {
		t.Fatal(err)
	}
	want := CrossMintActivationCapacity{Cluster: lease.Cluster, TargetReserve: lease.TargetReserve, LiquidityMint: lease.TargetLiquidityMint, ObservedSupplyUSDMicros: out.TargetObservedSupplyUSDMicros, ObservedSlot: out.ObservedSlot, MaximumInflightUSDMicros: 4_000_000, TelemetryVersion: 0}
	if p != want {
		t.Fatalf("capture invented or lost initial frontier: %+v want %+v", p, want)
	}
	out.ObservedSlot++
	out.TargetObservedSupplyUSDMicros = 500_000_000
	p, err = store.captureCrossMintActivationCapacity(ctx, out, lease)
	if err != nil {
		t.Fatal(err)
	}
	want.ObservedSlot, want.ObservedSupplyUSDMicros = out.ObservedSlot, out.TargetObservedSupplyUSDMicros
	want.MaximumInflightUSDMicros, want.TelemetryVersion = 10_000_000, 1
	if p != want {
		t.Fatalf("capture did not read source-updated exact version and maximum: %+v want %+v", p, want)
	}
	repeated, err := store.captureCrossMintActivationCapacity(ctx, out, lease)
	if err != nil || repeated != want {
		t.Fatalf("same observation changed telemetry version: %+v %v", repeated, err)
	}
	for name, mutate := range map[string]func(*CrossMintActivationPreparation){
		"conflict at same slot": func(p *CrossMintActivationPreparation) { p.TargetObservedSupplyUSDMicros++ },
		"older telemetry":       func(p *CrossMintActivationPreparation) { p.ObservedSlot-- },
		"expired source":        func(p *CrossMintActivationPreparation) { p.ObservedAt = time.Now().Add(-16 * time.Second) },
		"changed economics":     func(p *CrossMintActivationPreparation) { p.SourceAPYBPS++ },
	} {
		t.Run(name, func(t *testing.T) {
			bad := out
			mutate(&bad)
			if got, err := store.captureCrossMintActivationCapacity(ctx, bad, lease); err == nil || !reflect.DeepEqual(got, CrossMintActivationCapacity{}) {
				t.Fatalf("invalid observation returned usable capacity: %+v %v", got, err)
			}
			current, err := store.captureCrossMintActivationCapacity(ctx, out, lease)
			if err != nil || current != want {
				t.Fatalf("rejected observation mutated frontier: %+v %v", current, err)
			}
		})
	}
}

func TestCrossMintActivationCapacityRejectsBeforeCreatingFrontier(t *testing.T) {
	store, ctx := crossMintCapacityFixtureStore(t)
	out, lease := crossMintCapacityObservationFixture(time.Now())
	lease.Cluster = fmt.Sprintf("capture-refused-%d", time.Now().UnixNano())
	bad := out
	bad.TargetAPYBPS++
	if _, err := store.captureCrossMintActivationCapacity(ctx, bad, lease); err == nil {
		t.Fatal("mismatched epoch published telemetry")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.captureCrossMintActivationCapacity(canceled, out, lease); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled observation touched capacity or lost cancellation: %v", err)
	}
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.target_capacity_frontiers WHERE cluster=$1`, lease.Cluster).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected source created frontier: count=%d err=%v", count, err)
	}
}
