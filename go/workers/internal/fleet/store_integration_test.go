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

func openTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	databaseURL := os.Getenv("FLEET_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("FLEET_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	store, err := OpenStore(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return store, ctx
}

// testSnapshot holds each vault's collateral 1:1 in one reserve under a
// route policy that delegates exclusively to signer.
func testSnapshot(slot int64, reserves []ReserveState, signer string, holdings map[*RegisteredVault]ReserveIdentity, amount uint64) Snapshot {
	s := Snapshot{Slot: slot, ObservedAt: time.Now().UTC(), Reserves: map[string]ReserveState{}, Vaults: map[string]VaultFacts{}}
	for _, r := range reserves {
		s.Reserves[r.Address] = r
	}
	for v, reserve := range holdings {
		s.Vaults[v.VaultPubkey] = VaultFacts{Policy: &DecodedSquadsPolicy{Settings: v.Settings, AccountIndex: uint8(v.VaultIndex), DelegatedSigners: []string{signer}},
			Holdings: []Holding{{Reserve: reserve.Address, Market: reserve.Market, Mint: reserve.Mint, CollateralRaw: amount, LiquidityRaw: amount}}}
	}
	return s
}

func testReserve(identity ReserveIdentity, slot, apy int64) ReserveState {
	ample, limit := int64(2_000_000_000_000_000), uint64(1<<62)
	return ReserveState{ReserveIdentity: identity, Slot: slot, LastUpdateSlot: slot, SupplyAPYBPS: apy, TotalSupplyUSDMicros: 2_000_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: strings.Repeat("a", 64), AvailableLiquidityRaw: &ample, DepositLimitRaw: &limit}
}

func registeredVault(t *testing.T, registry []RegisteredVault, vaultID int64) *RegisteredVault {
	t.Helper()
	for i := range registry {
		if registry[i].VaultID == vaultID {
			return &registry[i]
		}
	}
	return nil
}

// The registry binds a vault's exact finalized Earn and swap policies; a
// holding read from the snapshot plans a cross-mint move under them, and the
// claim takes it with the bound withdraw policy.
func TestRegistryBindsFinalizedCrossMintPolicies(t *testing.T) {
	store, ctx := openTestStore(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	cluster, settings, authority := "cross-mint-"+suffix, "settings:"+suffix, "authority:"+suffix
	market, targetMarket := testIdentity(91), testIdentity(92)
	source := ReserveIdentity{Address: testIdentity(93), Market: market, Mint: USDCMint}
	target := ReserveIdentity{Address: testIdentity(94), Market: targetMarket, Mint: PYUSDMint}
	vaultID := seedWorkerVault(t, ctx, store, suffix, market, source.Address)
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.route_policies SET cluster=$2,source_commitment='finalized',finalized_eligible=true,stable_mints=ARRAY[$3,$4]::text[],kamino_markets=ARRAY[$5,$6]::text[],kamino_liquidity_mints=ARRAY[$3,$4]::text[] WHERE id=(SELECT active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1)`, vaultID, cluster, USDCMint, PYUSDMint, market, targetMarket); err != nil {
		t.Fatal(err)
	}
	// The finalized source Earn policy is intentionally not the vault's active
	// base policy. Planning may bind this newer exact policy for withdrawal.
	withdrawPolicyAccount := "withdraw:" + suffix
	if _, err := store.pool.Exec(ctx, `INSERT INTO loyal_yield.route_policies(cluster,settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,delegated_signers,threshold,route_modes,stable_mints,kamino_markets,kamino_liquidity_mints,swap_lanes,active,source_commitment,finalized_eligible,last_seen_slot,last_seen_signature) VALUES($1,$2,$3,2,$4,0,$5,ARRAY[$3]::text[],1,ARRAY['same_mint_kamino']::text[],ARRAY[$6]::text[],ARRAY[$7]::text[],ARRAY[$6]::text[],'[]',true,'finalized',true,1001,$8)`, cluster, settings, authority, withdrawPolicyAccount, "vault:"+suffix, USDCMint, market, "withdraw-signature:"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO loyal_yield.cross_mint_vault_opt_ins(cluster,settings,vault_index,vault_pubkey,enabled,classic_policy_account,classic_policy_seed,token_2022_policy_account,token_2022_policy_seed,max_slippage_bps,daily_source_mint_spending_cap,generation) VALUES($1,$2,0,$3,true,$4,11,$5,12,50,1000000000000,7)`, cluster, settings, "vault:"+suffix, "classic:"+suffix, "token2022:"+suffix); err != nil {
		t.Fatal(err)
	}
	for _, shard := range []struct {
		name, account string
		seed          int64
	}{{"classic", "classic:" + suffix, 11}, {"token_2022", "token2022:" + suffix, 12}} {
		if _, err := store.pool.Exec(ctx, `INSERT INTO loyal_yield.cross_mint_swap_policies(cluster,settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,delegated_signer,source_shard,max_slippage_bps,daily_source_mint_spending_cap,manifest_fingerprint,active,start_eligible,last_mutation,source_commitment,last_seen_slot,last_seen_signature) VALUES($1,$2,$3,$4,$5,0,$6,$3,$7,50,1000000000000,$8,true,true,'create','finalized',1000,$9)`, cluster, settings, authority, shard.seed, shard.account, "vault:"+suffix, shard.name, strings.Repeat("a", 64), "swap-signature:"+shard.name+suffix); err != nil {
			t.Fatal(err)
		}
	}
	options := FleetLoadOptions{DelegatedSigner: authority, EnableCrossMint: true, CrossMintMaxValueLossBPS: 50}
	registry, err := store.LoadRegistry(ctx, cluster, options)
	if err != nil {
		t.Fatal(err)
	}
	v := registeredVault(t, registry, vaultID)
	if v == nil {
		t.Fatalf("registry omitted the enrolled vault: %+v", registry)
	}
	reserves := []ReserveState{testReserve(source, 1000, 100), testReserve(target, 1000, 900)}
	snapshot := testSnapshot(1000, reserves, authority, map[*RegisteredVault]ReserveIdentity{v: source}, 777_000_000_000)
	vaults, err := PlanningVaults(snapshot, []ReserveIdentity{source, target}, []RegisteredVault{*v}, nil, nil, options)
	if err != nil || len(vaults) != 1 {
		t.Fatalf("planning vaults: %+v %v", vaults, err)
	}
	binding, ok := vaults[0].CrossMintTargets[target.Address]
	if !ok || binding.Withdraw.PolicyAccount != withdrawPolicyAccount || binding.Swap.SourceShard != "classic" || binding.Swap.EnrollmentGeneration != 7 || binding.Withdraw.ConstraintIndex != 0 || binding.Deposit.ConstraintIndex != 1 || binding.Withdraw.SourceCommitment != "finalized" || binding.Deposit.SourceCommitment != "finalized" {
		t.Fatalf("incomplete cross-mint binding: %+v", binding)
	}
	plan, err := PlanFleet(snapshot.Market("cross-mint-"+suffix), vaults)
	if err != nil || len(plan.Opportunities) != 1 || !bytes.Contains(plan.Opportunities[0].ExecutionPlan, []byte(`"route_kind":"cross_mint_jupiter"`)) {
		t.Fatalf("cross-mint plan was not executable: %+v %v", plan, err)
	}
	summary, err := store.PublishPlan(ctx, cluster, snapshot, plan)
	if err != nil || summary.Published != 1 {
		t.Fatalf("cross-mint publish failed: %+v %v", summary, err)
	}
	var queueMint, sourceMint, targetMint, routeKind string
	if err = store.pool.QueryRow(ctx, `SELECT liquidity_mint,source_liquidity_mint,target_liquidity_mint,execution_plan->>'route_kind' FROM loyal_yield.rebalance_opportunities WHERE cluster=$1 AND vault_id=$2`, cluster, vaultID).Scan(&queueMint, &sourceMint, &targetMint, &routeKind); err != nil {
		t.Fatal(err)
	}
	if queueMint != PYUSDMint || sourceMint != USDCMint || targetMint != PYUSDMint || routeKind != "cross_mint_jupiter" {
		t.Fatalf("cross-mint durable handoff drifted: queue=%s source=%s target=%s route=%s", queueMint, sourceMint, targetMint, routeKind)
	}
	lease, err := store.ClaimRevalidation(ctx, cluster, "bound-policy-revalidator", time.Minute, false, true, authority)
	if err != nil || lease == nil {
		t.Fatalf("claim cross-mint opportunity with distinct withdraw policy: %+v %v", lease, err)
	}
	if lease.PolicyAccount != withdrawPolicyAccount || !contains(lease.DelegatedSigners, authority) {
		t.Fatalf("claim used active base policy instead of bound withdraw policy: %+v", lease)
	}
}

// A policy on chain that no longer delegates exclusively to the signer plans
// nothing for that vault, and never fails another vault's plan.
func TestPlanningVaultsNeedTheLivePolicyOfEachVault(t *testing.T) {
	source := ReserveIdentity{Address: "source", Market: "market", Mint: USDCMint}
	target := ReserveIdentity{Address: "target", Market: "market", Mint: USDCMint}
	healthy := RegisteredVault{VaultID: 1, Settings: "settings-1", VaultPubkey: "vault-1", Markets: []string{"market"}, StableMints: []string{USDCMint}, KaminoMints: []string{USDCMint}}
	foreign := RegisteredVault{VaultID: 2, Settings: "settings-2", VaultPubkey: "vault-2", Markets: []string{"market"}, StableMints: []string{USDCMint}, KaminoMints: []string{USDCMint}}
	absent := RegisteredVault{VaultID: 3, Settings: "settings-3", VaultPubkey: "vault-3", Markets: []string{"market"}, StableMints: []string{USDCMint}, KaminoMints: []string{USDCMint}}
	snapshot := testSnapshot(1000, []ReserveState{testReserve(source, 1000, 100), testReserve(target, 1000, 900)}, "signer", map[*RegisteredVault]ReserveIdentity{&healthy: source, &foreign: source, &absent: source}, 1_000_000_000_000)
	snapshot.Vaults["vault-2"].Policy.DelegatedSigners = []string{"someone-else"}
	facts := snapshot.Vaults["vault-3"]
	facts.Policy, facts.PolicyErr = nil, "policy account is absent or not owned by Squads"
	snapshot.Vaults["vault-3"] = facts
	vaults, err := PlanningVaults(snapshot, []ReserveIdentity{source, target}, []RegisteredVault{healthy, foreign, absent}, nil, nil, FleetLoadOptions{DelegatedSigner: "signer"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanFleet(snapshot.Market("policies"), vaults)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Opportunities) != 1 || plan.Opportunities[0].Decision.VaultID != 1 || plan.Rejections[2] != "route_policy_not_delegated" || plan.Rejections[3] != "route_policy_unreadable" {
		t.Fatalf("policy facts: moves=%+v rejections=%v", plan.Opportunities, plan.Rejections)
	}
}

// The queue is the plan: the first tick publishes the vault's move, a racing
// publication of the same plan keeps it, a later plan holding the same move
// keeps it, a plan without it supersedes it, and claimed work is never
// touched.
func TestPublishPlanMakesTheQueueThePlan(t *testing.T) {
	store, ctx := openTestStore(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	cluster := "publish-" + suffix
	market, otherMarket := testIdentity(40), testIdentity(81)
	source := ReserveIdentity{Address: testIdentity(3), Market: market, Mint: USDCMint}
	target := ReserveIdentity{Address: testIdentity(80), Market: otherMarket, Mint: USDCMint}
	vaultID := seedWorkerVault(t, ctx, store, suffix, market, source.Address)
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.route_policies SET cluster=$2,source_commitment='finalized',finalized_eligible=true WHERE id=(SELECT active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1)`, vaultID, cluster); err != nil {
		t.Fatal(err)
	}
	signer := "authority:" + suffix
	catalog := []ReserveIdentity{source, target}
	plan := func(slot int64) (Snapshot, FleetPlan) {
		t.Helper()
		registry, err := store.LoadRegistry(ctx, cluster, FleetLoadOptions{DelegatedSigner: signer})
		if err != nil {
			t.Fatal(err)
		}
		v := registeredVault(t, registry, vaultID)
		if v == nil {
			return Snapshot{Slot: slot, ObservedAt: time.Now().UTC(), Reserves: map[string]ReserveState{}}, FleetPlan{}
		}
		snapshot := testSnapshot(slot, []ReserveState{testReserve(source, slot, 200), testReserve(target, slot, 700)}, signer, map[*RegisteredVault]ReserveIdentity{v: source}, 1_000_000_000_000)
		vaults, err := PlanningVaults(snapshot, catalog, []RegisteredVault{*v}, nil, nil, FleetLoadOptions{DelegatedSigner: signer})
		if err != nil {
			t.Fatal(err)
		}
		fleetPlan, err := PlanFleet(snapshot.Market(fmt.Sprintf("slot-%d", slot)), vaults)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot, fleetPlan
	}
	queue := func() map[int64]string {
		t.Helper()
		rows, err := store.pool.Query(ctx, `SELECT id,opportunity_state FROM loyal_yield.rebalance_opportunities WHERE cluster=$1`, cluster)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[int64]string{}
		for rows.Next() {
			var id int64
			var state string
			if err := rows.Scan(&id, &state); err != nil {
				t.Fatal(err)
			}
			out[id] = state
		}
		return out
	}
	// The target's market is outside the vault's policy: no move.
	if _, fleetPlan := plan(100); len(fleetPlan.Opportunities) != 0 || fleetPlan.Rejections[vaultID] != "no_eligible_target" {
		t.Fatalf("target outside the policy's markets was planned: %+v", fleetPlan)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.route_policies SET kamino_markets=array_append(kamino_markets,$2) WHERE id=(SELECT active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1)`, vaultID, otherMarket); err != nil {
		t.Fatal(err)
	}
	snapshot, fleetPlan := plan(100)
	if len(fleetPlan.Opportunities) != 1 {
		t.Fatalf("no move once the policy allows the target: %+v", fleetPlan)
	}
	// Even though deployment is strictly singleton, two publications of one
	// plan serialize on the vault: one inserts, the other keeps that row.
	type outcome struct {
		summary PublishSummary
		err     error
	}
	outcomes := make(chan outcome, 2)
	for range 2 {
		go func() {
			summary, err := store.PublishPlan(ctx, cluster, snapshot, fleetPlan)
			outcomes <- outcome{summary, err}
		}()
	}
	published, kept := int64(0), int64(0)
	for range 2 {
		o := <-outcomes
		if o.err != nil {
			t.Fatal(o.err)
		}
		published, kept = published+o.summary.Published, kept+o.summary.Kept
	}
	rows := queue()
	if published != 1 || kept != 1 || len(rows) != 1 {
		t.Fatalf("publication race was not serialized: published=%d kept=%d queue=%v", published, kept, rows)
	}
	var id int64
	for id = range rows {
	}
	var state string
	var marketSlot int64
	var executionPlan []byte
	if err := store.pool.QueryRow(ctx, `SELECT opportunity.opportunity_state,epoch.market_slot,opportunity.execution_plan FROM loyal_yield.rebalance_opportunities opportunity JOIN loyal_yield.optimizer_epochs epoch ON epoch.id=opportunity.optimizer_epoch_id WHERE opportunity.id=$1`, id).Scan(&state, &marketSlot, &executionPlan); err != nil {
		t.Fatal(err)
	}
	if state != "revalidate" || marketSlot != 100 {
		t.Fatalf("wrong durable handoff: state=%s slot=%d", state, marketSlot)
	}
	var executable map[string]any
	if err := json.Unmarshal(executionPlan, &executable); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"settings", "vault_index", "source_reserve", "target_reserve", "liquidity_mint", "amount_raw", "redeemable_source_liquidity_amount_raw", "idle_vault_liquidity_amount_raw", "idle_token_account", "optimizer_market_slot", "observed_target_apy_bps", "confidence_ppm", "expected_service_millis", "holding_horizon_seconds", "estimated_execution_cost_usd_micros", "writable_conflict_keys"} {
		if _, present := executable[field]; !present {
			t.Fatalf("execution_plan.%s is missing", field)
		}
	}
	position := fleetPlan.Opportunities[0].Decision
	if executable["route_amount_semantics"] != amountSemanticsRedeemableLiquidity || executable["source_amount_semantics"] != amountSemanticsKaminoCollateralDeposited ||
		int64(executable["redeemable_source_liquidity_amount_raw"].(float64)) != position.AmountRaw {
		t.Fatalf("amount contract drifted: %s", executionPlan)
	}
	conflicts, ok := executable["writable_conflict_keys"].([]any)
	var policyID int64
	if err := store.pool.QueryRow(ctx, `SELECT active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1`, vaultID).Scan(&policyID); err != nil {
		t.Fatal(err)
	}
	if !ok || len(conflicts) != 4 || conflicts[0] != "vault:vault:"+suffix || conflicts[1] != "policy:"+strconv.FormatInt(policyID, 10) ||
		conflicts[2] != "source-reserve:"+source.Address || conflicts[3] != "target-reserve:"+target.Address {
		t.Fatalf("conflict keys drifted: %s", executionPlan)
	}
	// A later tick planning the same move keeps the unclaimed row.
	snapshot, fleetPlan = plan(101)
	if summary, err := store.PublishPlan(ctx, cluster, snapshot, fleetPlan); err != nil || summary.Kept != 1 || summary.Published != 0 || summary.Superseded != 0 {
		t.Fatalf("an unchanged unclaimed move was not kept: %+v %v", summary, err)
	}
	// A claimed move is out of the plan's reach: the vault leaves the
	// registry and an empty plan supersedes nothing.
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET opportunity_state='leased',lease_kind='revalidate',lease_owner='executor',lease_expires_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	snapshot, fleetPlan = plan(102)
	if len(fleetPlan.Opportunities) != 0 {
		t.Fatalf("a vault with claimed work was planned: %+v", fleetPlan)
	}
	if summary, err := store.PublishPlan(ctx, cluster, snapshot, fleetPlan); err != nil || summary.Superseded != 0 || queue()[id] != "leased" {
		t.Fatalf("the plan touched claimed work: %+v %v %v", summary, err, queue())
	}
	// Released unclaimed, the move is superseded by a plan that no longer
	// holds it (the vault already holds the better reserve).
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET opportunity_state='revalidate',lease_kind=NULL,lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if summary, err := store.PublishPlan(ctx, cluster, Snapshot{Slot: 103, ObservedAt: time.Now().UTC(), Reserves: map[string]ReserveState{}}, FleetPlan{}); err != nil || summary.Superseded != 1 || queue()[id] != "superseded" {
		t.Fatalf("an unplanned move survived the plan: %+v %v %v", summary, err, queue())
	}
	// With nothing claimed the vault is planned and published again.
	snapshot, fleetPlan = plan(104)
	if summary, err := store.PublishPlan(ctx, cluster, snapshot, fleetPlan); err != nil || summary.Published != 1 {
		t.Fatalf("the vault was not planned again: %+v %v", summary, err)
	}
}
