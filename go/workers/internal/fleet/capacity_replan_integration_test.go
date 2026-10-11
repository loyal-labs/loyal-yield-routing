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

// Durable re-plan: a vault holding two reserves loads as two holdings; an
// admission whose source is already paying out an admitted move fails as a
// capacity re-plan inside the locked transaction; the re-plan ends the
// opportunity as stale/capacity_replan exactly once and the vault is planned
// again on the next load.
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
	// The second vault also holds a smaller secondary reserve.
	if _, err = store.pool.Exec(ctx, `INSERT INTO loyal_yield.vault_reserve_positions_current(vault_id,reserve,market,liquidity_mint,amount_raw,has_value,supply_apy_bps,snapshot_id,observed_slot,observed_at,planning_metadata)
SELECT vault_id,$2,market,liquidity_mint,90000000000,true,50,snapshot_id,observed_slot,clock_timestamp(),'{"amount_semantics":"kamino_obligation_collateral_deposited_amount","redeemable_source_liquidity_amount_raw":"100000000000"}'
FROM loyal_yield.vault_reserve_positions_current WHERE vault_id=$1`, second, secondary.Address); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	snapshot := MarketSnapshot{Cluster: cluster, Slot: 1000, ObservedAt: now, Reserves: map[string]ReserveState{
		source.Address:    {ReserveIdentity: source, Slot: 1000, LastUpdateSlot: 1000, SupplyAPYBPS: 100, TotalSupplyUSDMicros: 1_000_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: strings.Repeat("a", 64)},
		secondary.Address: {ReserveIdentity: secondary, Slot: 1000, LastUpdateSlot: 1000, SupplyAPYBPS: 50, TotalSupplyUSDMicros: 1_000_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: strings.Repeat("c", 64)},
		target.Address:    {ReserveIdentity: target, Slot: 1000, LastUpdateSlot: 1000, SupplyAPYBPS: 900, TotalSupplyUSDMicros: 1_000_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: strings.Repeat("b", 64)},
	}}
	ampleCapacity(snapshot)
	epoch := testImmutableMarketEpoch(t, snapshot, source, secondary, target)
	snapshot.Hash, snapshot.ExpiresAt = epoch.Fingerprint, epoch.ExpiresAt
	snapshot.MintExpiresAt = map[string]time.Time{USDCMint: epoch.OptimizerEnvelopeExpiresAt()}
	holdings := func() map[int64][]VaultPosition {
		t.Helper()
		fleet, err := store.LoadMigratedFleet(ctx, cluster, epoch)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64][]VaultPosition{}
		for _, vault := range fleet {
			out[vault.Position.VaultID] = append(out[vault.Position.VaultID], vault.Position)
		}
		return out
	}
	loaded := holdings()
	if got := loaded[second]; len(got) != 2 || got[0].SourceReserve != source.Address || got[1].SourceReserve != secondary.Address {
		t.Fatalf("two-holding vault loaded as %+v", got)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO loyal_yield.target_capacity_frontiers(cluster,target_reserve,liquidity_mint,observed_supply_usd_micros,observed_slot,maximum_inflight_usd_micros,telemetry_version) VALUES($1,$2,$3,$4,1000,$5,1)`, cluster, target.Address, USDCMint, int64(1_000_000_000_000_000), int64(20_000_000_000_000)); err != nil {
		t.Fatal(err)
	}
	// publishAndClaim plans the loaded fleet, publishes vault's move and
	// claims it for fused admission.
	publishAndClaim := func(vault int64) (*RevalidationLease, Decision) {
		t.Helper()
		fleet, err := store.LoadMigratedFleet(ctx, cluster, epoch)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := PlanFleet(snapshot, fleet)
		if err != nil {
			t.Fatal(err)
		}
		for _, opportunity := range plan.Opportunities {
			if opportunity.Decision.VaultID != vault {
				continue
			}
			var position VaultPosition
			for _, holding := range fleet {
				if holding.Position.VaultID == vault && holding.Position.SourceReserve == opportunity.Decision.SourceReserve {
					position = holding.Position
				}
			}
			published, err := store.Publish(ctx, cluster, epoch, position, opportunity.Decision)
			if err != nil || !published.Inserted {
				t.Fatalf("publish vault %d: %+v %v", vault, published, err)
			}
			lease, err := store.ClaimRevalidation(ctx, cluster, "replan-owner", time.Minute, false, false)
			if err != nil || lease == nil || lease.VaultID != vault {
				t.Fatalf("claim vault %d: %+v %v", vault, lease, err)
			}
			return lease, opportunity.Decision
		}
		t.Fatalf("vault %d has no planned move: %v", vault, plan.Rejections)
		return nil, Decision{}
	}
	commit := func(lease *RevalidationLease, sourceAvailable, targetRoom int64) error {
		wireHash := strings.Repeat("f", 64)
		prepared := &RoutePreparation{RouteFingerprint: strings.Repeat("1", 64), RequirementsFingerprint: strings.Repeat("2", 64), ExecutionPlan: json.RawMessage(`{"message_base64":"AQ==","unsigned_wire_base64":"Ag==","simulation":{"succeeded":true}}`), Transaction: PreparedTransaction{Message: []byte{1}, UnsignedWire: []byte{2}, WireSHA256: wireHash, PacketBytes: 1, FeeLamports: 1, ComputeLimit: 1}, Simulation: SimulationEvidence{Slot: 1001, Succeeded: true, UnitsConsumed: 1, WireSHA256: wireHash}}
		if err := preserveCanonicalPlan(lease.ExecutionPlan, prepared, "prepared_transaction"); err != nil {
			t.Fatal(err)
		}
		return store.commitRevalidation(ctx, *lease, RevalidationCommit{Disposition: "fused_execute", Preparation: prepared, ConflictKeys: []string{"vault:" + lease.VaultPubkey}, ExpectedEpochFingerprint: epoch.Fingerprint, ExpectedOpportunityKey: lease.IdempotencyKey, FreshEconomics: true, ObservedSourceAPYBPS: 100, ObservedTargetAPYBPS: 900, TargetObservedSupplyUSDMicros: 1_000_000_000_000_000, TargetObservedSlot: 1000, SourceAvailableLiquidityRaw: &sourceAvailable, TargetDepositRoomRaw: &targetRoom}, nil)
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
