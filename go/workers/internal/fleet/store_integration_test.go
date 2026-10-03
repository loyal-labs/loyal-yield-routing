package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLoadMigratedFleetBuildsFinalizedCrossMintPolicyBindings(t *testing.T) {
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
	cluster, settings, authority := "cross-mint-"+suffix, "settings:"+suffix, "authority:"+suffix
	market, targetMarket := testIdentity(91), testIdentity(92)
	source := ReserveIdentity{Address: testIdentity(93), Market: market, Mint: USDCMint}
	target := ReserveIdentity{Address: testIdentity(94), Market: targetMarket, Mint: PYUSDMint}
	vaultID := seedWorkerVault(t, ctx, store, suffix, market, source.Address)
	// Under explicit redeemable-liquidity semantics the projection amount is
	// authoritative; optional amount aliases must not be required.
	if _, err = store.pool.Exec(ctx, `UPDATE loyal_yield.vault_reserve_positions_current SET amount_raw=777000000000,planning_metadata='{"amount_semantics":"redeemable_liquidity_amount"}'::jsonb WHERE vault_id=$1`, vaultID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE loyal_yield.route_policies SET cluster=$2,source_commitment='finalized',finalized_eligible=true,stable_mints=ARRAY[$3,$4]::text[],kamino_markets=ARRAY[$5,$6]::text[],kamino_liquidity_mints=ARRAY[$3,$4]::text[] WHERE id=(SELECT active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1)`, vaultID, cluster, USDCMint, PYUSDMint, market, targetMarket); err != nil {
		t.Fatal(err)
	}
	// The finalized source Earn policy is intentionally not the vault's active
	// base policy. Planning may bind this newer exact policy for withdrawal.
	withdrawPolicyAccount := "withdraw:" + suffix
	if _, err = store.pool.Exec(ctx, `INSERT INTO loyal_yield.route_policies(cluster,settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,delegated_signers,threshold,route_modes,stable_mints,kamino_markets,kamino_liquidity_mints,swap_lanes,active,source_commitment,finalized_eligible,last_seen_slot,last_seen_signature) VALUES($1,$2,$3,2,$4,0,$5,ARRAY[$3]::text[],1,ARRAY['same_mint_kamino']::text[],ARRAY[$6]::text[],ARRAY[$7]::text[],ARRAY[$6]::text[],'[]',true,'finalized',true,1001,$8)`, cluster, settings, authority, withdrawPolicyAccount, "vault:"+suffix, USDCMint, market, "withdraw-signature:"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO loyal_yield.cross_mint_vault_opt_ins(cluster,settings,vault_index,vault_pubkey,enabled,classic_policy_account,classic_policy_seed,token_2022_policy_account,token_2022_policy_seed,max_slippage_bps,daily_source_mint_spending_cap,generation) VALUES($1,$2,0,$3,true,$4,11,$5,12,50,1000000000000,7)`, cluster, settings, "vault:"+suffix, "classic:"+suffix, "token2022:"+suffix); err != nil {
		t.Fatal(err)
	}
	for _, shard := range []struct {
		name, account string
		seed          int64
	}{{"classic", "classic:" + suffix, 11}, {"token_2022", "token2022:" + suffix, 12}} {
		if _, err = store.pool.Exec(ctx, `INSERT INTO loyal_yield.cross_mint_swap_policies(cluster,settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,delegated_signer,source_shard,max_slippage_bps,daily_source_mint_spending_cap,manifest_fingerprint,active,start_eligible,last_mutation,source_commitment,last_seen_slot,last_seen_signature) VALUES($1,$2,$3,$4,$5,0,$6,$3,$7,50,1000000000000,$8,true,true,'create','finalized',1000,$9)`, cluster, settings, authority, shard.seed, shard.account, "vault:"+suffix, shard.name, strings.Repeat("a", 64), "swap-signature:"+shard.name+suffix); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	snapshot := MarketSnapshot{Slot: 1000, ObservedAt: now, Reserves: map[string]ReserveState{
		source.Address: {ReserveIdentity: source, Slot: 1000, LastUpdateSlot: 1000, SupplyAPYBPS: 100, TotalSupplyUSDMicros: 1_000_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: strings.Repeat("a", 64)},
		target.Address: {ReserveIdentity: target, Slot: 1000, LastUpdateSlot: 1000, SupplyAPYBPS: 900, TotalSupplyUSDMicros: 1_000_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: strings.Repeat("b", 64)},
	}}
	epoch := testImmutableMarketEpoch(t, snapshot, source, target)
	fleet, err := store.LoadMigratedFleet(ctx, cluster, epoch, FleetLoadOptions{DelegatedSigner: authority, EnableCrossMint: true, CrossMintMaxValueLossBPS: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(fleet) != 1 {
		t.Fatalf("loaded %d vaults, want 1", len(fleet))
	}
	if fleet[0].Position.AmountRaw != 777_000_000_000 || fleet[0].Position.SourceCollateralAmountRaw != 0 {
		t.Fatalf("redeemable projection amount was not authoritative: %+v", fleet[0].Position)
	}
	binding, ok := fleet[0].CrossMintTargets[target.Address]
	if !ok || binding.Withdraw.PolicyAccount != withdrawPolicyAccount || binding.Swap.SourceShard != "classic" || binding.Swap.EnrollmentGeneration != 7 || binding.Withdraw.ConstraintIndex != 0 || binding.Deposit.ConstraintIndex != 1 || binding.Withdraw.SourceCommitment != "finalized" || binding.Deposit.SourceCommitment != "finalized" {
		t.Fatalf("incomplete cross-mint binding: %+v", binding)
	}
	snapshot.Cluster = cluster
	snapshot.Hash = epoch.Fingerprint
	snapshot.OptimizerEpochID, err = store.EnsureOptimizerEpoch(ctx, cluster, epoch)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.ExpiresAt = epoch.ExpiresAt
	snapshot.MintExpiresAt = map[string]time.Time{USDCMint: epoch.OptimizerEnvelopeExpiresAt(), PYUSDMint: epoch.OptimizerEnvelopeExpiresAt()}
	unanchoredPlan, err := PlanFleet(snapshot, fleet)
	if err != nil || len(unanchoredPlan.Opportunities) != 0 {
		t.Fatalf("cross-mint work without collateral anchor was admitted: %+v %v", unanchoredPlan, err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE loyal_yield.vault_reserve_positions_current SET planning_metadata='{"amount_semantics":"kamino_obligation_collateral_deposited_amount","redeemable_source_liquidity_amount_raw":"777000000000"}'::jsonb WHERE vault_id=$1`, vaultID); err != nil {
		t.Fatal(err)
	}
	fleet, err = store.LoadMigratedFleet(ctx, cluster, epoch, FleetLoadOptions{DelegatedSigner: authority, EnableCrossMint: true, CrossMintMaxValueLossBPS: 50})
	if err != nil || len(fleet) != 1 {
		t.Fatalf("reload anchored fleet: %+v %v", fleet, err)
	}
	plan, err := PlanFleet(snapshot, fleet)
	if err != nil || len(plan.Opportunities) != 1 || !bytes.Contains(plan.Opportunities[0].ExecutionPlan, []byte(`"route_kind":"cross_mint_jupiter"`)) {
		t.Fatalf("cross-mint plan was not executable: %+v %v", plan, err)
	}
	published, err := store.Publish(ctx, cluster, epoch, fleet[0].Position, plan.Opportunities[0].Decision)
	if err != nil || !published.Inserted {
		t.Fatalf("cross-mint publish failed: %+v decision=%+v error=%v", published, plan.Opportunities[0].Decision, err)
	}
	var queueMint, sourceMint, targetMint, key, routeKind string
	if err = store.pool.QueryRow(ctx, `SELECT liquidity_mint,source_liquidity_mint,target_liquidity_mint,idempotency_key,execution_plan->>'route_kind' FROM loyal_yield.rebalance_opportunities WHERE id=$1`, published.OpportunityID).Scan(&queueMint, &sourceMint, &targetMint, &key, &routeKind); err != nil {
		t.Fatal(err)
	}
	if queueMint != PYUSDMint || sourceMint != USDCMint || targetMint != PYUSDMint || key != plan.Opportunities[0].IdempotencyKey || routeKind != "cross_mint_jupiter" {
		t.Fatalf("cross-mint durable handoff drifted: queue=%s source=%s target=%s key=%s route=%s", queueMint, sourceMint, targetMint, key, routeKind)
	}
	lease, err := store.ClaimRevalidation(ctx, cluster, "bound-policy-revalidator", time.Minute, false, true, authority)
	if err != nil || lease == nil {
		t.Fatalf("claim cross-mint opportunity with distinct withdraw policy: %+v %v", lease, err)
	}
	if lease.PolicyAccount != withdrawPolicyAccount || !contains(lease.DelegatedSigners, authority) {
		t.Fatalf("claim used active base policy instead of bound withdraw policy: %+v", lease)
	}
}

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
		} else if outcome.result.Reason == "rust_identity_duplicate" {
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
	if duplicate.Inserted || duplicate.OpportunityID != 0 || duplicate.Reason != "active_work" {
		t.Fatalf("new epoch was not fenced by existing active work: %+v", duplicate)
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
