package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStoreIntegrationDurableHandoffWithoutPlannerMigration(t *testing.T) {
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
	target := ReserveIdentity{Address: testIdentity(80), Market: testIdentity(81), Mint: USDCMint}
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
	_, err = store.pool.Exec(ctx, `INSERT INTO loyal_yield.vault_reserve_positions_current(vault_id,reserve,market,liquidity_mint,amount_raw,has_value,supply_apy_bps,snapshot_id,observed_slot,observed_at,planning_metadata) VALUES($1,$2,$3,$4,900000000000,true,200,$5,99,clock_timestamp(),'{"amount_semantics":"kamino_obligation_collateral_deposited_amount","redeemable_source_liquidity_amount_raw":"1000000000000"}')`, vaultID, source.Address, market, USDCMint, snapshotID)
	if err != nil {
		t.Fatal(err)
	}

	var plannerTableExists bool
	if err := store.pool.QueryRow(ctx, `SELECT to_regclass('loyal_yield.kamino_fleet_planner_owners') IS NOT NULL`).Scan(&plannerTableExists); err != nil {
		t.Fatal(err)
	}
	if plannerTableExists {
		t.Fatal("cutover verifier unexpectedly depends on a planner-specific migration")
	}
	policyBlocked, err := store.LoadVaultPosition(ctx, "mainnet-beta", vaultID, source, target)
	if err != nil || !strings.Contains(policyBlocked.BlockedReason, "market_not_allowed") {
		t.Fatalf("target market outside policy was accepted: position=%+v err=%v", policyBlocked, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.route_policies SET kamino_markets=array_append(kamino_markets,$2) WHERE id=$1`, policyID, target.Market); err != nil {
		t.Fatal(err)
	}
	position, err := store.LoadVaultPosition(ctx, "mainnet-beta", vaultID, source, target)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	snapshot := MarketSnapshot{Slot: 100, ObservedAt: now, Hash: "snapshot-" + suffix, Reserves: map[string]ReserveState{
		source.Address: {ReserveIdentity: source, Slot: 100, LastUpdateSlot: 100, SupplyAPYBPS: 200, TotalSupplyUSDMicros: 2_000_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: strings.Repeat("a", 64)},
		target.Address: {ReserveIdentity: target, Slot: 100, LastUpdateSlot: 100, SupplyAPYBPS: 700, TotalSupplyUSDMicros: 2_000_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: strings.Repeat("b", 64)}}}
	epoch := testImmutableMarketEpoch(t, snapshot, source, target)
	decision := Plan(snapshot, position, source.Address, target.Address)
	if !decision.Eligible {
		t.Fatalf("decision ineligible: %s", decision.Reason)
	}
	// Even though deployment is strictly singleton, the generic queue mutex must
	// fail safe if two admission calls race during an operational mistake.
	type publishOutcome struct {
		result PublishResult
		err    error
	}
	outcomes := make(chan publishOutcome, 2)
	for range 2 {
		go func() {
			result, publishErr := store.Publish(ctx, "mainnet-beta", epoch, position, decision)
			outcomes <- publishOutcome{result: result, err: publishErr}
		}()
	}
	var published PublishResult
	inserted, duplicates := 0, 0
	for range 2 {
		outcome := <-outcomes
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		if outcome.result.Inserted {
			inserted++
			published = outcome.result
		} else if outcome.result.Reason == "economic_duplicate" {
			duplicates++
		}
	}
	if inserted != 1 || duplicates != 1 || published.OpportunityID <= 0 || published.EpochID <= 0 {
		t.Fatalf("publication race was not serialized: inserted=%d duplicates=%d published=%+v", inserted, duplicates, published)
	}
	var state, epochFingerprint string
	var marketSlot int64
	var executionPlan []byte
	err = store.pool.QueryRow(ctx, `SELECT opportunity.opportunity_state,epoch.market_slot,epoch.market_state->>'fingerprint',opportunity.execution_plan FROM loyal_yield.rebalance_opportunities opportunity JOIN loyal_yield.optimizer_epochs epoch ON epoch.id=opportunity.optimizer_epoch_id WHERE opportunity.id=$1`, published.OpportunityID).Scan(&state, &marketSlot, &epochFingerprint, &executionPlan)
	if err != nil {
		t.Fatal(err)
	}
	if state != "revalidate" || marketSlot != 100 || epochFingerprint != epoch.Fingerprint {
		t.Fatalf("wrong durable handoff: state=%s slot=%d fingerprint=%s", state, marketSlot, epochFingerprint)
	}
	var plan map[string]any
	if err := json.Unmarshal(executionPlan, &plan); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"settings", "vault_index", "source_reserve", "target_reserve", "liquidity_mint", "amount_raw", "redeemable_source_liquidity_amount_raw", "idle_vault_liquidity_amount_raw", "idle_token_account", "optimizer_market_slot", "observed_target_apy_bps", "confidence_ppm", "expected_service_millis", "holding_horizon_seconds", "estimated_execution_cost_usd_micros", "writable_conflict_keys"} {
		if _, present := plan[field]; !present {
			t.Fatalf("Rust W3 required execution_plan.%s is missing", field)
		}
	}
	if plan["route_amount_semantics"] != amountSemanticsRedeemableLiquidity ||
		plan["source_amount_semantics"] != amountSemanticsKaminoCollateralDeposited ||
		int64(plan["redeemable_source_liquidity_amount_raw"].(float64)) != decision.AmountRaw {
		t.Fatalf("Rust W3 amount contract drifted: %s", executionPlan)
	}
	conflicts, ok := plan["writable_conflict_keys"].([]any)
	if !ok || len(conflicts) != 4 || conflicts[0] != "vault:"+position.VaultPubkey ||
		conflicts[1] != "policy:"+strconv.FormatInt(position.PolicyID, 10) ||
		conflicts[2] != "source-reserve:"+source.Address || conflicts[3] != "target-reserve:"+target.Address {
		t.Fatalf("Rust W3 conflict keys drifted: %s", executionPlan)
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
	epoch = testImmutableMarketEpoch(t, snapshot, source, target)
	duplicate, err := store.Publish(ctx, "mainnet-beta", epoch, position, decision)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.Inserted || duplicate.OpportunityID != published.OpportunityID || duplicate.Reason != "economic_duplicate" {
		t.Fatalf("economic retry was not idempotent: %+v", duplicate)
	}
	// A fresh process recovers from the authoritative queue, not a private
	// planner watermark. Existing active work must still block replanning.
	restarted, err := OpenStore(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	recovered, err := restarted.LoadVaultPosition(ctx, "mainnet-beta", vaultID, source, target)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.BlockedReason != "active_opportunity" {
		t.Fatalf("restart did not recover durable active work: %+v", recovered)
	}
}
