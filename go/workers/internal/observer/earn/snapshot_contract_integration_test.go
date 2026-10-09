package earn

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/watch"
	"github.com/solana-foundation/solana-go/v2"
)

// Binding recovery enqueues unsigned state reads. SnapshotApplicable must name
// exactly the shapes the durable consumer can complete: every shape it
// admits completes on the first attempt, and every shape it rejects is one
// the consumer can only fail until the dead-letter ceiling.
func TestSnapshotApplicableMatchesUnsignedJobOutcome(t *testing.T) {
	dsn := os.Getenv("EARN_PARITY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("EARN_PARITY_TEST_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	app := &Application{store: NewStore(pool), consumer: "earn-smart-account:snapshot-" + strconv.FormatInt(time.Now().UnixNano(), 10)}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.earn_reconciliation_jobs WHERE consumer_name=$1`, app.consumer)
		_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.laserstream_replay_cursors WHERE consumer_name=$1`, app.consumer)
	})
	key := func() string { return solana.NewWallet().PublicKey().String() }
	vault := func(earnMax bool, accounts ...watch.Account) watch.Vault {
		index := uint8(1)
		if earnMax {
			index = 0
		}
		return watch.Vault{Environment: "mainnet-beta", Settings: key(), Wallet: key(), Vault: key(), VaultIndex: index, EarnMax: earnMax, Accounts: accounts}
	}
	policy, idle := key(), key()
	maxVault := vault(true, watch.Account{Pubkey: policy, Role: "policy"})
	maxKey := solana.MustPublicKeyFromBase58(maxVault.Vault)
	custody := associatedToken(maxKey, usdcMint, tokenProgram).String()
	maxVault.Accounts = append(maxVault.Accounts, watch.Account{Pubkey: custody, Role: "idle_token"})
	classic := vault(false, watch.Account{Pubkey: policy, Role: "policy"}, watch.Account{Pubkey: idle, Role: "idle_token"})
	for _, shape := range []struct {
		name    string
		vault   watch.Vault
		address string
		filter  string
		deleted bool
	}{
		{"earn_max_custody", maxVault, custody, watch.EarnIdleTokenAccounts, false},
		{"earn_max_policy_deleted", maxVault, policy, watch.EarnPolicyAccounts, true},
		{"classic_policy_deleted", classic, policy, watch.EarnPolicyAccounts, true},
		{"classic_policy_present", classic, policy, watch.EarnPolicyAccounts, false},
		{"classic_idle_deleted", classic, idle, watch.EarnIdleTokenAccounts, true},
	} {
		t.Run(shape.name, func(t *testing.T) {
			kind := "account"
			if shape.deleted {
				kind = "account_deleted"
			}
			address, eventKey := shape.address, "snapshot:"+shape.name
			update := NormalizedUpdate{EventKey: &eventKey, Filters: []string{shape.filter}, EventKind: kind, AccountPubkey: &address, Slot: 500}
			if _, err := app.store.Enqueue(ctx, app.consumer, eventKey, 500, update, []watch.Vault{shape.vault}, address); err != nil {
				t.Fatal(err)
			}
			outcome, err := app.processNextJob(ctx, "snapshot-owner")
			if err != nil || outcome.idle {
				t.Fatalf("job not processed: %+v %v", outcome, err)
			}
			completed := !outcome.deferred && !outcome.deadLettered
			failing := outcome.deferred && outcome.kind == deferFailure
			if applicable := SnapshotApplicable(shape.vault, shape.address, shape.deleted); applicable != completed || (!applicable && !failing) {
				t.Fatalf("SnapshotApplicable=%v but unsigned job outcome %+v", applicable, outcome)
			}
		})
	}
}
