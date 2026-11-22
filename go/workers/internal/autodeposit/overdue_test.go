package autodeposit

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

func TestOverdueRequiresCurrentCustodyOrASelectedClaim(t *testing.T) {
	dsn := os.Getenv("AUTODEPOSIT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires a disposable autodeposit_overdue_test database")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnConfig.Database != "autodeposit_overdue_test" || !strings.HasPrefix(cfg.ConnConfig.Host, "/tmp/") {
		t.Fatal("requires the disposable database on a local test socket")
	}
	ctx := t.Context()
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("CREATE SCHEMA loyal_yield")
	defer exec("DROP SCHEMA loyal_yield CASCADE")
	exec(`
CREATE TABLE loyal_yield.balance_sweep_targets (id bigint, vault_pubkey text, vault_token_ata text, token_mint text, desired_active bool, chain_status text, wallet_balance_floor_raw bigint);
CREATE TABLE loyal_yield.balance_sweep_scheduled_slots (id bigint, target_id bigint, token_mint text, status text, eligible_after timestamptz, idle_blocked_since timestamptz, last_error text, claim_token text, execution_id bigint, updated_at timestamptz);
CREATE UNIQUE INDEX one_scheduled_slot ON loyal_yield.balance_sweep_scheduled_slots (target_id) WHERE status = 'scheduled';
CREATE TABLE loyal_yield.balance_sweep_wallet_balances_current (target_id bigint, mint text, amount_raw bigint);
CREATE TYPE loyal_yield.balance_sweep_surplus_classification AS ENUM ('unknown');
CREATE TABLE loyal_yield.balance_sweep_surplus_lots (id bigserial, scheduled_slot_id bigint, target_id bigint, status text,
    remaining_amount_raw bigint, original_amount_raw bigint, source_event_id bigint UNIQUE, source_signature text,
    classification loyal_yield.balance_sweep_surplus_classification, eligible_after timestamptz, confidence text, reason text);
CREATE TABLE loyal_yield.balance_sweep_lot_claims (target_id bigint, claim_token text, status text, created_at timestamptz);
CREATE TABLE loyal_yield.balance_sweep_transaction_attempts (id bigint, scheduled_slot_id bigint, attempt_state text, created_at timestamptz);
CREATE TABLE loyal_yield.managed_vaults (id bigint, vault_pubkey text);
CREATE TABLE loyal_yield.rebalance_decisions (vault_id bigint, movement_route text, status text, terminal_outcome text, custody_account text, created_at timestamptz);`)
	exec(`INSERT INTO loyal_yield.balance_sweep_targets VALUES (1, 'vault', 'vault-ata', $1, true, 'active', 100000)`, USDCMint)
	exec(`
INSERT INTO loyal_yield.balance_sweep_scheduled_slots (id,target_id,token_mint,status,eligible_after,idle_blocked_since,last_error)
SELECT 2,id,token_mint,'scheduled',now()+interval '1 hour',now()-interval '71 days','existing idle vault balance must drain' FROM loyal_yield.balance_sweep_targets;
INSERT INTO loyal_yield.balance_sweep_wallet_balances_current SELECT id,token_mint,101000 FROM loyal_yield.balance_sweep_targets;
INSERT INTO loyal_yield.balance_sweep_surplus_lots (scheduled_slot_id,target_id,status,remaining_amount_raw) VALUES (2,1,'open',1000);
INSERT INTO loyal_yield.managed_vaults VALUES (3,'vault');`)

	reg := prometheus.NewRegistry()
	w := Worker{store: &Store{pool: db}, facts: engine.NewFacts(reg)}
	checkPage := func(want bool) {
		t.Helper()
		if err := w.passFacts(ctx); err != nil {
			t.Fatal(err)
		}
		if age := metricValue(t, reg, "loyal_autodeposit_oldest_due_lot_age_seconds", nil); (age > 3600) != want {
			t.Fatalf("age %v seconds, want overdue %v", age, want)
		}
	}
	checkPage(false)
	exec(`INSERT INTO loyal_yield.rebalance_decisions VALUES (3,'cross_mint_jupiter','confirming',NULL,NULL,now())`)
	checkPage(false)
	deferSlot := func(reason string, custody bool) {
		t.Helper()
		exec(`UPDATE loyal_yield.balance_sweep_scheduled_slots SET eligible_after = now()`)
		if ok, err := w.store.DeferScheduledSlot(ctx, 1, 2, reason, custody); !ok || err != nil {
			t.Fatalf("defer slot: %v, %v", ok, err)
		}
	}
	deferSlot(ownedCustodyDeferral, true)
	checkPage(false) // The newly observed owner does not inherit the dust timer.
	exec(`UPDATE loyal_yield.balance_sweep_scheduled_slots SET idle_blocked_since = now() - interval '2 hours'`)
	deferSlot(ownedCustodyDeferral, true)
	checkPage(true) // The next retry is in the future, but custody is still held.
	exec(`DELETE FROM loyal_yield.rebalance_decisions`)
	checkPage(false)
	exec(`INSERT INTO loyal_yield.rebalance_decisions VALUES (3,'cross_mint_jupiter','confirming',NULL,NULL,now())`)
	credit := func(eventID, after int64) {
		t.Helper()
		exec(`UPDATE loyal_yield.balance_sweep_wallet_balances_current SET amount_raw = $1`, after)
		floor := int64(100000)
		event := walletBalanceEventRow{EventID: eventID, AmountRaw: after, ObservedAt: time.Now(), TargetActive: true, WalletBalanceFloorRaw: &floor}
		err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
			target, err := lockTarget(ctx, tx, 1)
			if err != nil {
				return err
			}
			_, err = insertPositiveDeltaLot(ctx, tx, target, event, 1000)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	credit(10, 102000) // Existing spendable funds are still blocked.
	checkPage(true)
	exec(`UPDATE loyal_yield.balance_sweep_wallet_balances_current SET amount_raw = 100000`)
	checkPage(false)
	credit(11, 101000) // This transfer supplied all the spendable funds.
	checkPage(false)
	deferSlot(ownedCustodyDeferral, true)
	exec(`UPDATE loyal_yield.balance_sweep_scheduled_slots SET idle_blocked_since = now() - interval '2 hours'`)
	deferSlot("delegation unavailable", false)
	checkPage(false)
	exec(`INSERT INTO loyal_yield.balance_sweep_lot_claims VALUES (1,'claim','selected',now()-interval '2 hours')`)
	checkPage(true)

	exec(`UPDATE loyal_yield.rebalance_decisions SET status = 'confirmed'`)
	checkHeld := func(claim string, sinceAttempt int64, want bool) {
		t.Helper()
		held, err := w.store.VaultCustodyHeldElsewhere(ctx, "vault", "vault-ata", claim, sinceAttempt)
		if err != nil || held != want {
			t.Fatalf("custody held for claim %q after attempt %d: got %v, want %v, err %v", claim, sinceAttempt, held, want, err)
		}
	}
	checkHeld("claim", 0, false)
	checkHeld("another-claim", 0, true)
	exec(`INSERT INTO loyal_yield.balance_sweep_transaction_attempts VALUES (4,2,'confirmed',now()-interval '1 day')`)
	checkHeld("claim", 4, true)
}
