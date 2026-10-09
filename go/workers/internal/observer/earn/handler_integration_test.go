package earn

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/watch"
	"github.com/solana-foundation/solana-go/v2"
)

func TestHandlerAtomicallyEnqueuesJobsAutodepositAndCursor(t *testing.T) {
	databaseURL := os.Getenv("OBSERVER_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OBSERVER_TEST_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	endpoint, parseErr := url.Parse(databaseURL)
	if parseErr != nil || endpoint.Hostname() != "127.0.0.1" || endpoint.User == nil || endpoint.User.Username() != "workers_v2" || endpoint.Path != "/workers_v2_observer" {
		t.Fatal("observer integration requires the isolated workers_v2 fixture")
	}
	settings, vault, account := solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey()
	var targetID int64
	err = pool.QueryRow(ctx, `INSERT INTO loyal_yield.balance_sweep_targets
        (settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,wallet,
         wallet_usdc_ata,vault_usdc_ata,wallet_token_ata,vault_token_ata,token_mint,threshold,
         max_amount_per_period,desired_active,chain_status,last_seen_slot,last_seen_signature)
        VALUES($1,$1,1,$3,1,$2,$1,$1,$2,$1,$2,$4,1,1000,true,'active',1,'fixture') RETURNING id`,
		settings.String(), vault.String(), account.String(), "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v").Scan(&targetID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.earn_reconciliation_jobs WHERE settings=$1`, settings.String())
		_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.balance_sweep_targets WHERE id=$1`, targetID)
	})
	store := NewStore(pool)
	handler := NewHandler(store, "mainnet")
	handler.SetWatchSet(&watch.Set{Vaults: []watch.Vault{{Environment: "mainnet", Settings: settings.String(), Vault: vault.String(), VaultIndex: 1, Accounts: []watch.Account{{Pubkey: account.String(), Role: "policy"}}}}})
	update := &pb.SubscribeUpdate{Filters: []string{watch.EarnPolicyAccounts}, UpdateOneof: &pb.SubscribeUpdate_Account{Account: &pb.SubscribeUpdateAccount{Slot: 88, Account: &pb.SubscribeUpdateAccountInfo{Pubkey: account[:], Lamports: 1, TxnSignature: make([]byte, 64)}}}}
	first, err := handler.HandleAccount(ctx, update)
	if err != nil {
		t.Fatal(err)
	}
	if first.InsertedJobs != 1 || first.CoalescedAutodeposits != 1 || first.Cursor != 88 {
		t.Fatalf("first enqueue = %+v", first)
	}
	applied, err := store.ApplicationCursor(ctx, handler.ConsumerName())
	if err != nil || applied >= first.Cursor {
		t.Fatalf("capture outran application: applied=%d capture=%d err=%v", applied, first.Cursor, err)
	}
	duplicate, err := handler.HandleAccount(ctx, update)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.InsertedJobs != 0 {
		t.Fatalf("duplicate enqueue inserted %d jobs", duplicate.InsertedJobs)
	}
	var jobs, cursor, requested int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.earn_reconciliation_jobs WHERE settings=$1`, settings.String()).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT durable_slot FROM loyal_yield.laserstream_replay_cursors WHERE consumer_name=$1`, handler.ConsumerName()).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT requested_slot FROM loyal_yield.autodeposit_reconciliation_requests WHERE target_id=$1`, targetID).Scan(&requested); err != nil {
		t.Fatal(err)
	}
	// A newer coalesced demand must survive replay of the earlier event.
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.autodeposit_reconciliation_requests SET requested_slot=99 WHERE target_id=$1`, targetID); err != nil {
		t.Fatal(err)
	}
	if _, err := handler.HandleAccount(ctx, update); err != nil {
		t.Fatal(err)
	}
	var highwater int64
	if err := pool.QueryRow(ctx, `SELECT requested_slot FROM loyal_yield.autodeposit_reconciliation_requests WHERE target_id=$1`, targetID).Scan(&highwater); err != nil {
		t.Fatal(err)
	}
	if highwater != 99 {
		t.Fatalf("older replay replaced newer request: %d", highwater)
	}
	// A SQL failure after the first insert must leave neither its job nor cursor.
	_, err = pool.Exec(ctx, `CREATE FUNCTION loyal_yield.workers_v2_capture_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.consumer_name='rollback-fixture' THEN RAISE EXCEPTION 'fixture cursor failure'; END IF; RETURN NEW; END $$;
    CREATE TRIGGER workers_v2_capture_fail BEFORE INSERT OR UPDATE ON loyal_yield.laserstream_replay_cursors FOR EACH ROW EXECUTE FUNCTION loyal_yield.workers_v2_capture_fail();`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS workers_v2_capture_fail ON loyal_yield.laserstream_replay_cursors; DROP FUNCTION IF EXISTS loyal_yield.workers_v2_capture_fail();`)
	})
	invalidVaults := []watch.Vault{{Settings: settings.String(), Vault: vault.String(), VaultIndex: 1}}
	if _, err := store.Enqueue(ctx, "rollback-fixture", "rollback-fixture", 100, NormalizedUpdate{Slot: 100}, invalidVaults, account.String()); err == nil {
		t.Fatal("injected cursor failure was accepted")
	}
	var rollbackJobs int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.earn_reconciliation_jobs WHERE consumer_name='rollback-fixture'`).Scan(&rollbackJobs); err != nil {
		t.Fatal(err)
	}
	rollbackCursor, err := store.ReplayCursor(ctx, "rollback-fixture")
	if err != nil || rollbackJobs != 0 || rollbackCursor != 0 {
		t.Fatalf("partial capture committed jobs=%d cursor=%d error=%v", rollbackJobs, rollbackCursor, err)
	}
	if jobs != 1 || cursor != 88 || requested != 88 {
		t.Fatalf("durable state jobs=%d cursor=%d requested=%d", jobs, cursor, requested)
	}
	if err := store.AdvanceReplayCursor(ctx, "watch-observation", 75); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceReplayCursor(ctx, "watch-observation", 50); err != nil {
		t.Fatal(err)
	}
	watchCursor, err := store.ReplayCursor(ctx, "watch-observation")
	if err != nil {
		t.Fatal(err)
	}
	if watchCursor != 75 {
		t.Fatalf("watch observation cursor = %d, want monotonic 75", watchCursor)
	}
}
