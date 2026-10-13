package fleet

import (
	"context"
	"encoding/binary"
	"testing"
)

// seedWorkerVault seeds an active vault enrolled for same-mint routing under
// a policy whose authority is its delegated signer, allowing USDC in market.
// The chain holds its funds; tests hand the planner a position directly.
func seedWorkerVault(t *testing.T, ctx context.Context, store *Store, suffix, market, _ string) int64 {
	t.Helper()
	var policyID, vaultID int64
	err := store.pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_policies(settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,delegated_signers,threshold,route_modes,stable_mints,kamino_markets,kamino_liquidity_mints,swap_lanes,active,last_seen_slot,last_seen_signature) VALUES($1,$2,1,$3,0,$4,ARRAY[$2]::text[],1,ARRAY['same_mint_kamino']::text[],ARRAY[$5]::text[],ARRAY[$6]::text[],ARRAY[$5]::text[],'[]',true,999,$7) RETURNING id`, `settings:`+suffix, `authority:`+suffix, `policy:`+suffix, `vault:`+suffix, USDCMint, market, `signature:`+suffix).Scan(&policyID)
	if err != nil {
		t.Fatal(err)
	}
	err = store.pool.QueryRow(ctx, `INSERT INTO loyal_yield.managed_vaults(settings,vault_index,vault_pubkey,active_policy_id,active) VALUES($1,0,$2,$3,true) RETURNING id`, `settings:`+suffix, `vault:`+suffix, policyID).Scan(&vaultID)
	if err != nil {
		t.Fatal(err)
	}
	return vaultID
}

// loadTestPosition is the vault's holding in source as the planner builds it
// from a snapshot: 900,000 collateral redeeming 1,000,000 USDC.
func loadTestPosition(t *testing.T, ctx context.Context, store *Store, vaultID int64, source ReserveIdentity) VaultPosition {
	t.Helper()
	p := VaultPosition{VaultID: vaultID, SourceReserve: source.Address, Market: source.Market, Mint: source.Mint,
		AmountRaw: 1_000_000_000_000, SourceCollateralAmountRaw: 900_000_000_000, SourceAmountSemantics: amountSemanticsKaminoCollateralDeposited, ObservedSlot: 999}
	if err := store.pool.QueryRow(ctx, `SELECT vault.settings,vault.vault_index,vault.vault_pubkey,policy.id,policy.policy_account,policy.authority,clock_timestamp()
FROM loyal_yield.managed_vaults vault JOIN loyal_yield.route_policies policy ON policy.id=vault.active_policy_id WHERE vault.id=$1`, vaultID).Scan(&p.Settings, &p.VaultIndex, &p.VaultPubkey, &p.PolicyID, &p.PolicyAccount, &p.PolicyAuthority, &p.ObservedAt); err != nil {
		t.Fatal(err)
	}
	return p
}

// publishTestMove publishes the move Plan makes for position as the planner's
// tick would, and returns its opportunity.
func publishTestMove(t *testing.T, ctx context.Context, store *Store, cluster string, snapshot MarketSnapshot, position VaultPosition, target string) (int64, Decision) {
	t.Helper()
	d := Plan(snapshot, position, position.SourceReserve, target)
	if !d.Eligible {
		t.Fatalf("fixture move ineligible: %s", d.Reason)
	}
	plan, err := canonicalExecutionPlan(snapshot, FleetVault{Position: position}, d)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := store.PublishPlan(ctx, cluster, Snapshot{Slot: snapshot.Slot, ObservedAt: snapshot.ObservedAt, Reserves: snapshot.Reserves}, FleetPlan{Opportunities: []PlannedOpportunity{{Decision: d, ExecutionPlan: plan}}})
	if err != nil || summary.Published != 1 {
		t.Fatalf("publish: %+v %v", summary, err)
	}
	var id int64
	if err := store.pool.QueryRow(ctx, `SELECT id FROM loyal_yield.rebalance_opportunities WHERE cluster=$1 AND vault_id=$2 AND opportunity_state='revalidate'`, cluster, position.VaultID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id, d
}

func setCurveScale(data []byte, scale uint32) {
	config := data[4856:]
	for index := 0; index < 11; index++ {
		offset := 64 + index*8
		binary.LittleEndian.PutUint32(config[offset+4:offset+8], uint32(index)*scale)
	}
}
