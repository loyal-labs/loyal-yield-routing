package autodeposit

import (
	"context"
	"testing"
	"time"
)

// seedEarnWithdrawal records the vault's own Earn withdrawal carried by the
// wallet event's signature, as the Earn observer does.
func seedEarnWithdrawal(t *testing.T, store *Store, suffix, signature string, amountRaw int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `
INSERT INTO loyal_yield.user_yield_position_withdrawals
    (withdrawal_signature, confirmed_slot, wallet_address, smart_account_address, settings, vault_index,
     vault_pubkey, policy_id, policy_account, policy_seed, target_reserve, liquidity_mint,
     withdrawn_amount_raw, mode, confirmed_at, created_at)
VALUES ($1, 1, $2, $3, $4, 1, $3, 1, 'itest-policy', 7, $5, $6, $7, 'partial', now(), now())`,
		signature, "itest-wallet-"+suffix, "itest-vault-"+suffix, "itest-settings-"+suffix,
		defaultEarnReserve, USDCMint, amountRaw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.pool.Exec(context.Background(), `DELETE FROM loyal_yield.user_yield_position_withdrawals WHERE withdrawal_signature = $1`, signature)
	})
}

// The user's own Earn withdrawal landing in the wallet is no inflow: a lot
// for it would deposit the withdrawal straight back into yield. A plain
// inflow on the same target still schedules.
func TestProjectionRefusesOwnEarnWithdrawalInflow(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	seeded := seedIntegrationTarget(t, store, "withdrawal-inflow")
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	// insertIntegrationEvent signs each event itest-sig-<event id>.
	seedEarnWithdrawal(t, store, "withdrawal-inflow", "itest-sig-9200002", 10_000_000)
	store.insertIntegrationEvent(t, seeded.TargetID, 9_200_001, 4_000_000, nil, base)
	store.insertIntegrationEvent(t, seeded.TargetID, 9_200_002, 14_000_000, ptrInt64(10_000_000), base.Add(time.Minute))
	store.insertIntegrationEvent(t, seeded.TargetID, 9_200_003, 15_000_000, ptrInt64(1_000_000), base.Add(2*time.Minute))

	outcome, err := store.ProjectSurplusLotsOnce(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var lots []int64
	if err := store.pool.QueryRow(ctx, `SELECT COALESCE(array_agg(source_event_id ORDER BY source_event_id), '{}') FROM loyal_yield.balance_sweep_surplus_lots WHERE target_id = $1`, seeded.TargetID).Scan(&lots); err != nil {
		t.Fatal(err)
	}
	if outcome.EventsScanned != 3 || outcome.LotsCreated != 1 || len(lots) != 1 || lots[0] != 9_200_003 {
		t.Fatalf("projection outcome %+v lots from events %v, want only the plain 1 USDC inflow scheduled", outcome, lots)
	}
}
