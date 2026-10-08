package autodeposit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	WorkersDB "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
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

// When Earn records the withdrawal after the wallet event was projected, the
// lot it produced is retracted in Earn's transaction. Another vault's lot
// with the same signature is not this withdrawal's.
func TestEarnWithdrawalRetractsItsProjectedLot(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	own := seedIntegrationTarget(t, store, "withdrawal-own")
	other := seedIntegrationTarget(t, store, "withdrawal-other")
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	signature := "itest-retracted-withdrawal"
	for i, target := range []int64{own.TargetID, other.TargetID} {
		eventID := int64(9_300_001 + i)
		store.insertIntegrationEvent(t, target, eventID, 14_000_000, ptrInt64(10_000_000), base)
		if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_wallet_balance_events SET txn_signature = $2 WHERE event_id = $1`, eventID, signature); err != nil {
			t.Fatal(err)
		}
	}
	if outcome, err := store.ProjectSurplusLotsOnce(ctx, 100); err != nil || outcome.LotsCreated != 2 {
		t.Fatalf("projection before Earn knew the withdrawal: %+v %v, want both lots", outcome, err)
	}
	if err := WorkersDB.WithTx(ctx, store.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return SuppressWithdrawalLots(ctx, tx, signature, "itest-settings-withdrawal-own", 1, "itest-vault-withdrawal-own")
	}); err != nil {
		t.Fatal(err)
	}
	for target, want := range map[int64]string{own.TargetID: "suppressed", other.TargetID: "open"} {
		var status string
		if err := store.pool.QueryRow(ctx, `SELECT status::text FROM loyal_yield.balance_sweep_surplus_lots WHERE target_id = $1`, target).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != want {
			t.Errorf("target %d lot %s, want %s", target, status, want)
		}
	}
}
