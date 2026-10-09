package fleetexec

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	sdk "github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// barrierSweepRPC holds every vault read until the whole wave is in flight,
// so the wave reaches its database writes at once.
type barrierSweepRPC struct {
	scriptedSweepRPC
	wave    int
	arrived atomic.Int64
	release chan struct{}
	once    sync.Once
}

func (r *barrierSweepRPC) Accounts(ctx context.Context, keys []sdk.PublicKey, commitment rpc.CommitmentType, minContextSlot uint64) (uint64, []*chain.Account, error) {
	if len(keys) > 2 { // a vault batch, not the catalog universe read
		if r.arrived.Add(1) == int64(r.wave) {
			r.once.Do(func() { close(r.release) })
		}
		select {
		case <-r.release:
		case <-time.After(10 * time.Second):
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		}
	}
	return r.scriptedSweepRPC.Accounts(ctx, keys, commitment, minContextSlot)
}

// A 40-vault RPC wave on the production-sized 16-connection yield pool holds
// at most positionSweepDatabaseSlots connections, so another lane's query
// gets a connection immediately while every vault of the wave publishes.
func TestPositionSweepWaveLeavesTheSharedPoolToOtherLanes(t *testing.T) {
	config, err := pgxpool.ParseConfig(integrationURL(t))
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 16
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	requireDisposableDatabase(t, pool)
	store := &Store{pool: pool}
	suffix := fmt.Sprint(time.Now().UnixNano())
	cluster := "sweep-pool:" + suffix
	signer := sweepKey(suffix + ":signer")
	markets := [2]string{sweepKey(suffix + ":market-a"), sweepKey(suffix + ":market-b")}
	reserves := [2]string{sweepKey(suffix + ":reserve-a"), sweepKey(suffix + ":reserve-b")}
	seedSweepCatalog(t, ctx, pool, cluster, map[string]string{reserves[0]: "reserve", reserves[1]: "reserve"})
	const wave = 40
	for i := 0; i < wave; i++ {
		var policyID int64
		settings, vault := fmt.Sprintf("sweep-pool-settings:%s:%d", suffix, i), sweepKey(fmt.Sprintf("%s:vault:%d", suffix, i))
		if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_policies(settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,delegated_signers,threshold,route_modes,stable_mints,kamino_markets,kamino_liquidity_mints,swap_lanes,active,last_seen_slot,last_seen_signature) VALUES($1,$2,1,$3,0,$4,ARRAY[$2]::text[],1,ARRAY['same_mint_kamino']::text[],ARRAY[$5]::text[],ARRAY[$6,$7]::text[],ARRAY[$5]::text[],'[]',true,900,$8) RETURNING id`,
			settings, signer, sweepKey(settings+":policy"), vault, fleet.USDCMint, markets[0], markets[1], "sig:"+settings).Scan(&policyID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.managed_vaults(settings,vault_index,vault_pubkey,active_policy_id,active) VALUES($1,0,$2,$3,true)`, settings, vault, policyID); err != nil {
			t.Fatal(err)
		}
	}
	rpc := &barrierSweepRPC{scriptedSweepRPC: scriptedSweepRPC{slot: 1_000, accounts: map[string]chain.Account{
		reserves[0]: sweepReserve(reserves[0], markets[0]),
		reserves[1]: sweepReserve(reserves[1], markets[1]),
	}}, wave: wave, release: make(chan struct{})}
	sweep, err := NewPositionSweep(PositionSweepConfig{Cluster: cluster, DelegatedSigner: signer, EnabledMints: fleet.EarnStableMints(), Interval: time.Minute, Concurrency: wave, Facts: testFacts()}, store, rpc)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var observers sync.WaitGroup
	var peak, laneQueries atomic.Int64
	var slowestLane atomic.Int64
	observers.Add(2)
	go func() {
		defer observers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if acquired := int64(pool.Stat().AcquiredConns()); acquired > peak.Load() {
				peak.Store(acquired)
			}
		}
	}()
	go func() { // another lane on the same pool
		defer observers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			started := time.Now()
			laneCtx, cancel := context.WithTimeout(ctx, time.Second)
			var one int
			err := pool.QueryRow(laneCtx, `SELECT 1`).Scan(&one)
			cancel()
			if err != nil {
				t.Errorf("another lane could not query during the wave: %v", err)
				return
			}
			laneQueries.Add(1)
			if wait := time.Since(started).Microseconds(); wait > slowestLane.Load() {
				slowestLane.Store(wait)
			}
		}
	}()
	metrics, err := sweep.sweep(ctx)
	close(stop)
	observers.Wait()
	if err != nil || metrics.Refreshed != wave {
		t.Fatalf("wave did not publish every vault: %+v %v", metrics, err)
	}
	// The sweep may hold four connections and the other lane one of its own:
	// at least eleven of the sixteen stay free for Autodeposit, Multiply,
	// lookup and the fleet's own lanes throughout the wave.
	if got := peak.Load(); got > 5 {
		t.Fatalf("a %d-vault wave left only %d of 16 pool connections free", wave, 16-got)
	}
	if laneQueries.Load() == 0 {
		t.Fatal("the other lane never ran during the wave")
	}
	t.Logf("peak acquired=%d other-lane queries=%d slowest=%dµs", peak.Load(), laneQueries.Load(), slowestLane.Load())
}
