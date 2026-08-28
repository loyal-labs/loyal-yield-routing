package earn

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/watch"
)

func TestHandlerAtomicallyEnqueuesJobsAutodepositAndCursor(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	_, err = pool.Exec(ctx, `
		DROP SCHEMA IF EXISTS loyal_yield CASCADE; CREATE SCHEMA loyal_yield;
		CREATE TABLE loyal_yield.earn_reconciliation_jobs(id bigserial primary key,consumer_name text not null,event_key text not null,durable_slot bigint not null,settings text not null,vault_index smallint not null,vault_pubkey text not null,event_payload jsonb not null,vault_payload jsonb not null,attempt_count int not null default 0,next_attempt_at timestamptz not null default now(),claim_owner text,claim_expires_at timestamptz,last_error text,completed_at timestamptz,created_at timestamptz not null default now(),updated_at timestamptz not null default now(),unique(consumer_name,event_key,settings,vault_index,vault_pubkey));
		CREATE TABLE loyal_yield.laserstream_replay_cursors(consumer_name text primary key,durable_slot bigint not null,updated_at timestamptz not null default now());
		CREATE TABLE loyal_yield.autodeposit_reconciliation_requests(target_id bigint primary key,requested_slot bigint not null,next_attempt_at timestamptz not null default now(),updated_at timestamptz not null default now());
		CREATE TABLE loyal_yield.balance_sweep_targets(id bigint primary key,settings text,vault_pubkey text,chain_status text,policy_seed bigint,policy_account text,subscription_authority text,recurring_delegation text,wallet_token_ata text);
	`)
	if err != nil {
		t.Fatal(err)
	}
	settings, vault, account := solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey()
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.balance_sweep_targets VALUES(7,$1,$2,'active',1,$3,NULL,NULL,NULL)`, settings.String(), vault.String(), account.String()); err != nil {
		t.Fatal(err)
	}
	store := NewStore(pool)
	handler := NewHandler(store, "mainnet")
	handler.SetWatchSet(&watch.Set{Vaults: []watch.Vault{{Environment: "mainnet", Settings: settings.String(), Vault: vault.String(), VaultIndex: 1, Accounts: []watch.Account{{Pubkey: account.String(), Role: "policy"}}}}})
	update := &pb.SubscribeUpdate{Filters: []string{watch.EarnPolicyAccounts}, UpdateOneof: &pb.SubscribeUpdate_Account{Account: &pb.SubscribeUpdateAccount{Slot: 88, Account: &pb.SubscribeUpdateAccountInfo{Pubkey: account[:], Lamports: 1, TxnSignature: []byte{4, 5, 6}}}}}
	first, err := handler.HandleAccount(ctx, update)
	if err != nil {
		t.Fatal(err)
	}
	if first.InsertedJobs != 1 || first.CoalescedAutodeposits != 1 || first.Cursor != 88 {
		t.Fatalf("first enqueue = %+v", first)
	}
	duplicate, err := handler.HandleAccount(ctx, update)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.InsertedJobs != 0 {
		t.Fatalf("duplicate enqueue inserted %d jobs", duplicate.InsertedJobs)
	}
	var jobs, cursor, requested int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.earn_reconciliation_jobs`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT durable_slot FROM loyal_yield.laserstream_replay_cursors WHERE consumer_name=$1`, handler.ConsumerName()).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT requested_slot FROM loyal_yield.autodeposit_reconciliation_requests WHERE target_id=7`).Scan(&requested); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 || cursor != 88 || requested != 88 {
		t.Fatalf("durable state jobs=%d cursor=%d requested=%d", jobs, cursor, requested)
	}
}
