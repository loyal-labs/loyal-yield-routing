package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestStoreIntegrationDurableHandoffAndLeaseFencing(t *testing.T) {
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
	market := testIdentity(40)
	source := ReserveIdentity{Address: testIdentity(3), Market: market, Mint: USDCMint}
	target := ReserveIdentity{Address: testIdentity(80), Market: market, Mint: USDCMint}
	var policyID, vaultID, snapshotID int64
	err = store.pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_policies (settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,delegated_signers,threshold,route_modes,stable_mints,kamino_markets,kamino_liquidity_mints,swap_lanes,active,last_seen_slot,last_seen_signature) VALUES ($1,$2,1,$3,0,$4,ARRAY[$2]::text[],1,ARRAY['same_mint_kamino']::text[],ARRAY[$5]::text[],ARRAY[$6]::text[],ARRAY[$5]::text[],'[]',true,100,$7) RETURNING id`, `settings:`+suffix, `authority:`+suffix, `policy:`+suffix, `vault:`+suffix, USDCMint, market, `signature:`+suffix).Scan(&policyID)
	if err != nil {
		t.Fatal(err)
	}
	err = store.pool.QueryRow(ctx, `INSERT INTO loyal_yield.managed_vaults(settings,vault_index,vault_pubkey,active_policy_id,active) VALUES($1,0,$2,$3,true) RETURNING id`, `settings:`+suffix, `vault:`+suffix, policyID).Scan(&vaultID)
	if err != nil {
		t.Fatal(err)
	}
	err = store.pool.QueryRow(ctx, `INSERT INTO loyal_yield.vault_position_snapshots(vault_id,policy_id,observed_slot,observed_at,is_current,context) VALUES($1,$2,99,clock_timestamp(),true,'{}') RETURNING id`, vaultID, policyID).Scan(&snapshotID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.pool.Exec(ctx, `INSERT INTO loyal_yield.vault_reserve_positions_current(vault_id,reserve,market,liquidity_mint,amount_raw,has_value,supply_apy_bps,snapshot_id,observed_slot,observed_at,planning_metadata) VALUES($1,$2,$3,$4,9000000000000,true,200,$5,99,clock_timestamp(),'{"amount_semantics":"kamino_collateral_deposited","redeemable_source_liquidity_amount_raw":"10000000000000"}')`, vaultID, source.Address, market, USDCMint, snapshotID)
	if err != nil {
		t.Fatal(err)
	}

	lease, err := store.Acquire(ctx, "mainnet-beta", "integration:"+suffix, "owner-a", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Acquire(ctx, "mainnet-beta", "integration:"+suffix, "owner-b", 30*time.Second); err == nil {
		t.Fatal("overlapping owner acquired an unexpired lease")
	}
	position, err := store.LoadVaultPosition(ctx, "mainnet-beta", vaultID, source, target)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	snapshot := MarketSnapshot{Slot: 100, ObservedAt: now, Hash: "snapshot-" + suffix, Reserves: map[string]ReserveState{
		source.Address: {ReserveIdentity: source, Slot: 100, SupplyAPYBPS: 200, TotalSupplyUSDMicros: 2_000_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: "a"},
		target.Address: {ReserveIdentity: target, Slot: 100, SupplyAPYBPS: 700, TotalSupplyUSDMicros: 2_000_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: "b"}}}
	decision := Plan(snapshot, position, source.Address, target.Address)
	if !decision.Eligible {
		t.Fatalf("decision ineligible: %s", decision.Reason)
	}
	published, err := store.Publish(ctx, lease, snapshot, position, decision)
	if err != nil {
		t.Fatal(err)
	}
	if !published.Inserted || published.OpportunityID <= 0 || published.EpochID <= 0 {
		t.Fatalf("not published: %+v", published)
	}
	var state, owner string
	var marketSlot int64
	var executionPlan []byte
	err = store.pool.QueryRow(ctx, `SELECT opportunity.opportunity_state,epoch.market_slot,epoch.market_state->>'owner',opportunity.execution_plan FROM loyal_yield.rebalance_opportunities opportunity JOIN loyal_yield.optimizer_epochs epoch ON epoch.id=opportunity.optimizer_epoch_id WHERE opportunity.id=$1`, published.OpportunityID).Scan(&state, &marketSlot, &owner, &executionPlan)
	if err != nil {
		t.Fatal(err)
	}
	if state != "revalidate" || marketSlot != 100 || owner != "kamino_fleet_planner_go_v1" {
		t.Fatalf("wrong durable handoff: state=%s slot=%d owner=%s", state, marketSlot, owner)
	}
	var plan map[string]any
	if err := json.Unmarshal(executionPlan, &plan); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"settings", "vault_index", "source_reserve", "target_reserve", "liquidity_mint", "amount_raw", "redeemable_source_liquidity_amount_raw", "optimizer_market_slot", "observed_target_apy_bps", "confidence_ppm", "expected_service_millis", "holding_horizon_seconds", "estimated_execution_cost_usd_micros"} {
		if _, present := plan[field]; !present {
			t.Fatalf("Rust W3 required execution_plan.%s is missing", field)
		}
	}
	if plan["route_amount_semantics"] != "redeemable_liquidity_amount" || int64(plan["redeemable_source_liquidity_amount_raw"].(float64)) != decision.AmountRaw {
		t.Fatalf("Rust W3 amount contract drifted: %s", executionPlan)
	}
	blocked, err := store.LoadVaultPosition(ctx, "mainnet-beta", vaultID, source, target)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.BlockedReason != "active_opportunity" {
		t.Fatalf("active durable work did not block replanning: %+v", blocked)
	}
	snapshot.Slot = 101
	snapshot.ObservedAt = snapshot.ObservedAt.Add(time.Second)
	snapshot.Hash = "slot-churn-" + suffix
	for address, reserve := range snapshot.Reserves {
		reserve.Slot = 101
		snapshot.Reserves[address] = reserve
	}
	decision = Plan(snapshot, position, source.Address, target.Address)
	duplicate, err := store.Publish(ctx, lease, snapshot, position, decision)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.Inserted || duplicate.OpportunityID != published.OpportunityID || duplicate.Reason != "economic_duplicate" {
		t.Fatalf("economic retry was not idempotent: %+v", duplicate)
	}
	if err := store.RecordSnapshot(ctx, lease, snapshot, true); err != nil {
		t.Fatal(err)
	}
	if err := store.Release(ctx, lease); err != nil {
		t.Fatal(err)
	}
	next, err := store.Acquire(ctx, "mainnet-beta", "integration:"+suffix, "owner-b", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Release(ctx, next)
	if next.FencingToken <= lease.FencingToken || next.LastConfirmedSlot != 101 || next.LastSnapshotHash != snapshot.Hash {
		t.Fatalf("recovery watermark/fence was not retained: %+v", next)
	}
	if _, err := store.Publish(ctx, lease, snapshot, position, decision); err == nil || !strings.Contains(err.Error(), "lease was lost") {
		t.Fatalf("stale owner was not fenced: %v", err)
	}
}
