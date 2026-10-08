package fleet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestFleetMissingAmountDoesNotBlockHealthyVault(t *testing.T) {
	raw := os.Getenv("FLEET_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("FLEET_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	store, err := OpenStore(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	suffix := fmt.Sprint(time.Now().UnixNano())
	cluster := "amount-isolation-" + suffix
	signer := "signer:" + suffix
	source := ReserveIdentity{Address: testIdentity(80), Market: testIdentity(81), Mint: USDCMint}
	target := ReserveIdentity{Address: testIdentity(82), Market: source.Market, Mint: USDCMint}
	bad := seedWorkerVault(t, ctx, store, "bad:"+suffix, source.Market, source.Address)
	good := seedWorkerVault(t, ctx, store, "good:"+suffix, source.Market, source.Address)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE loyal_yield.route_policies SET cluster=$3,source_commitment='finalized',finalized_eligible=true,delegated_signers=ARRAY[$4]::text[] WHERE id IN (SELECT active_policy_id FROM loyal_yield.managed_vaults WHERE id IN ($1,$2))`, bad, good, cluster, signer)
	exec(`UPDATE loyal_yield.vault_reserve_positions_current SET amount_raw=1000000000,planning_metadata='{"amount_semantics":"redeemable_liquidity_amount"}'::jsonb WHERE vault_id=$1`, good)
	now := time.Now().UTC()
	snapshot := MarketSnapshot{Slot: 1000, ObservedAt: now, Reserves: map[string]ReserveState{}}
	for i, identity := range []ReserveIdentity{source, target} {
		snapshot.Reserves[identity.Address] = ReserveState{ReserveIdentity: identity, Slot: 1000, LastUpdateSlot: 999, SupplyAPYBPS: int64(100 + i*800), TotalSupplyUSDMicros: 1_000_000_000_000_000, EconomicLifetimeMillis: 300_000, DataHash: fmt.Sprintf("%064x", i+1)}
	}
	epoch := testImmutableMarketEpoch(t, snapshot, source, target)
	for _, metadata := range []string{`{"amount_semantics":"kamino_obligation_collateral_deposited_amount"}`, `{"amount_semantics":"unknown"}`, `[]`} {
		exec(`UPDATE loyal_yield.vault_reserve_positions_current SET planning_metadata=$2::jsonb WHERE vault_id=$1`, bad, metadata)
		rows, err := store.LoadMigratedFleet(ctx, cluster, epoch, FleetLoadOptions{DelegatedSigner: signer})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 {
			t.Fatalf("lost source accounting: %d", len(rows))
		}
		for _, v := range rows {
			if v.Position.VaultID == good && (v.Position.BlockedReason != "" || v.Position.AmountRaw != 1_000_000_000) {
				t.Fatal("bad row contaminated healthy amount parsing")
			}
		}
		plan, err := PlanFleet(snapshot, rows)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Opportunities) != 1 || plan.Opportunities[0].Decision.VaultID != good {
			t.Fatalf("healthy vault was not planned: %+v", plan)
		}
		if plan.Rejections[bad] != "unsupported_source_amount_evidence" && plan.Rejections[bad] != "invalid_source_planning_metadata" {
			t.Fatalf("missing explicit skip reason: %+v", plan.Rejections)
		}
	}
}
