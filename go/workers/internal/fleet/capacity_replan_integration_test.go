package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// Durable re-plan: a vault holding two reserves plans as two holdings; an
// admission whose source is already paying out an admitted move fails as a
// capacity re-plan inside the locked transaction; the re-plan ends the
// opportunity as stale/capacity_replan exactly once and the vault is planned
// again on the next tick.
func TestReplanIntegrationEndsTheMoveAndFreesItsVault(t *testing.T) {
	databaseURL := os.Getenv("FLEET_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("FLEET_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	store, err := OpenStore(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	suffix := fmt.Sprint(time.Now().UnixNano())
	cluster := "replan-" + suffix
	market := testIdentity(45)
	source := ReserveIdentity{Address: testIdentity(46), Market: market, Mint: USDCMint}
	secondary := ReserveIdentity{Address: testIdentity(47), Market: market, Mint: USDCMint}
	target := ReserveIdentity{Address: testIdentity(48), Market: market, Mint: USDCMint}
	first := seedWorkerVault(t, ctx, store, suffix+"-a", market, source.Address)
	second := seedWorkerVault(t, ctx, store, suffix+"-b", market, source.Address)
	catalog := []ReserveIdentity{source, secondary, target}
	reserves := []ReserveState{testReserve(source, 1000, 100), testReserve(secondary, 1000, 50), testReserve(target, 1000, 900)}
	// planned is one tick: the registry, the chain's holdings (the second
	// vault also holds a smaller secondary reserve) and the moves in flight.
	planned := func() ([]FleetVault, FleetPlan, Snapshot) {
		t.Helper()
		registry, err := store.LoadRegistry(ctx, cluster, FleetLoadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		holders := map[*RegisteredVault]ReserveIdentity{}
		for _, id := range []int64{first, second} {
			if v := registeredVault(t, registry, id); v != nil {
				holders[v] = source
			}
		}
		snapshot := testSnapshot(1000, reserves, "", holders, 1_000_000_000_000)
		for v := range holders {
			if v.VaultID == second {
				facts := snapshot.Vaults[v.VaultPubkey]
				facts.Holdings = append(facts.Holdings, Holding{Reserve: secondary.Address, Market: market, Mint: USDCMint, CollateralRaw: 100_000_000_000, LiquidityRaw: 100_000_000_000})
				snapshot.Vaults[v.VaultPubkey] = facts
			}
		}
		inflow, outflow, err := InflightFlows(ctx, store.pool, cluster, 1000, 0)
		if err != nil {
			t.Fatal(err)
		}
		vaults, err := PlanningVaults(snapshot, catalog, registry, inflow, outflow, FleetLoadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		plan, err := PlanFleet(snapshot.Market("replan"), vaults)
		if err != nil {
			t.Fatal(err)
		}
		return vaults, plan, snapshot
	}
	holdings := func() map[int64][]VaultPosition {
		t.Helper()
		vaults, _, _ := planned()
		out := map[int64][]VaultPosition{}
		for _, vault := range vaults {
			if vault.Position.VaultID == first || vault.Position.VaultID == second {
				out[vault.Position.VaultID] = append(out[vault.Position.VaultID], vault.Position)
			}
		}
		return out
	}
	loaded := holdings()
	if got := loaded[second]; len(got) != 2 || got[0].SourceReserve != source.Address || got[1].SourceReserve != secondary.Address {
		t.Fatalf("two-holding vault loaded as %+v", got)
	}
	// publishAndClaim plans one tick, publishes the plan and claims vault's
	// move for admission.
	publishAndClaim := func(vault int64) (*RevalidationLease, Decision) {
		t.Helper()
		_, plan, snapshot := planned()
		var move *PlannedOpportunity
		for i := range plan.Opportunities {
			if plan.Opportunities[i].Decision.VaultID == vault {
				move = &plan.Opportunities[i]
			}
		}
		if move == nil {
			t.Fatalf("vault %d has no planned move: %v", vault, plan.Rejections)
		}
		if _, err := store.PublishPlan(ctx, cluster, snapshot, FleetPlan{Opportunities: []PlannedOpportunity{*move}}); err != nil {
			t.Fatalf("publish vault %d: %v", vault, err)
		}
		lease, err := store.ClaimRevalidation(ctx, cluster, "replan-owner", time.Minute, false, false)
		if err != nil || lease == nil || lease.VaultID != vault {
			t.Fatalf("claim vault %d: %+v %v", vault, lease, err)
		}
		return lease, move.Decision
	}
	commit := func(lease *RevalidationLease, sourceAvailable, targetRoom int64) error {
		wireHash := strings.Repeat("f", 64)
		prepared := &RoutePreparation{RouteFingerprint: strings.Repeat("1", 64), RequirementsFingerprint: strings.Repeat("2", 64), ExecutionPlan: json.RawMessage(`{"message_base64":"AQ==","unsigned_wire_base64":"Ag==","simulation":{"succeeded":true}}`), Transaction: PreparedTransaction{Message: []byte{1}, UnsignedWire: []byte{2}, WireSHA256: wireHash, PacketBytes: 1, FeeLamports: 1, ComputeLimit: 1}, Simulation: SimulationEvidence{Slot: 1001, Succeeded: true, UnitsConsumed: 1, WireSHA256: wireHash}}
		if err := preserveCanonicalPlan(lease.ExecutionPlan, prepared, "prepared_transaction"); err != nil {
			t.Fatal(err)
		}
		return store.commitRevalidation(ctx, *lease, RevalidationCommit{Disposition: "fused_execute", Preparation: prepared, ConflictKeys: []string{"vault:" + lease.VaultPubkey}, ExpectedOpportunityKey: lease.IdempotencyKey, FreshEconomics: true, ObservedSourceAPYBPS: 100, ObservedTargetAPYBPS: 900, TargetObservedSupplyUSDMicros: 1_000_000_000_000_000, TargetObservedSlot: 1000, SourceAvailableLiquidityRaw: &sourceAvailable, TargetDepositRoomRaw: &targetRoom}, nil)
	}
	const ample = int64(1_000_000_000_000_000)
	firstLease, firstMove := publishAndClaim(first)
	if err = commit(firstLease, ample, ample); err != nil {
		t.Fatalf("admit the first move: %v", err)
	}
	if _, present := holdings()[first]; present {
		t.Fatal("a vault with an admitted move was planned again")
	}
	secondLease, secondMove := publishAndClaim(second)
	if secondMove.SourceReserve != source.Address {
		t.Fatalf("second vault planned %s->%s, want its primary holding", secondMove.SourceReserve, secondMove.TargetReserve)
	}
	// The source pays both moves only if it holds both: one micro short.
	err = commit(secondLease, firstMove.PrincipalUSDMicros+secondMove.PrincipalUSDMicros-1, ample)
	var replan *CapacityReplan
	if !errors.As(err, &replan) || replan.Reason != "source_liquidity_exhausted" {
		t.Fatalf("admission ignored the source's admitted outflow: %v", err)
	}
	replanned, err := store.ReplanRevalidation(ctx, *secondLease)
	if err != nil || !replanned {
		t.Fatalf("re-plan: %v %v", replanned, err)
	}
	var state, reason string
	if err = store.pool.QueryRow(ctx, `SELECT opportunity_state,terminal_reason FROM loyal_yield.rebalance_opportunities WHERE id=$1`, secondLease.OpportunityID).Scan(&state, &reason); err != nil || state != "stale" || reason != "capacity_replan" {
		t.Fatalf("re-planned opportunity is %s/%s (%v)", state, reason, err)
	}
	if again, err := store.ReplanRevalidation(ctx, *secondLease); err != nil || again {
		t.Fatalf("a finished lease re-planned twice: %v %v", again, err)
	}
	if got := holdings()[second]; len(got) != 2 {
		t.Fatalf("re-planned vault is not planned again: %+v", got)
	}
}
