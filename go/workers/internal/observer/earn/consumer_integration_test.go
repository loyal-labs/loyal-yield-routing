package earn

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/watch"
)

// The durable queue: a terminal failure is retried until the dead-letter
// ceiling, a decided Noop completes, and observer progress follows only the
// runnable backlog (a job deferred to a later attempt does not stall it).
func TestJobConsumerDeadLettersAndBacklogProgress(t *testing.T) {
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
	if _, err := pool.Exec(ctx, "TRUNCATE loyal_yield.earn_reconciliation_jobs, loyal_yield.laserstream_replay_cursors RESTART IDENTITY"); err != nil {
		t.Fatal(err)
	}
	app := &Application{store: NewStore(pool), consumer: "earn-smart-account:test"}
	vault := watch.Vault{Environment: "mainnet-beta", Settings: "settings", Wallet: "wallet", Vault: "vault", VaultIndex: 0, Accounts: []watch.Account{{Pubkey: "idle", Role: "idle_token"}}}
	account := "idle"
	noop := NormalizedUpdate{Filters: []string{watch.EarnIdleTokenAccounts}, EventKind: "account", AccountPubkey: &account, Slot: 5}
	if _, err := app.store.Enqueue(ctx, app.consumer, "noop", 5, noop, []watch.Vault{vault}, ""); err != nil {
		t.Fatal(err)
	}
	if outcome, err := app.processNextJob(ctx, "owner"); err != nil || outcome.applied || outcome.deferred || outcome.idle {
		t.Fatalf("an unsigned account update is a completed Noop: %+v %v", outcome, err)
	}
	vault.Vault = "other-vault"
	if _, err := app.store.Enqueue(ctx, app.consumer, "broken", 6, json.RawMessage(`"not an event"`), []watch.Vault{vault}, ""); err != nil {
		t.Fatal(err)
	}
	outcome, err := app.processNextJob(ctx, "owner")
	if err != nil || !outcome.deferred || outcome.kind != deferFailure || outcome.attempt != 1 {
		t.Fatalf("first failure = %+v %v", outcome, err)
	}
	if ok, err := app.caughtUp(ctx); err != nil || !ok {
		t.Fatalf("a job deferred to its next attempt stalled progress: %v %v", ok, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE loyal_yield.earn_reconciliation_jobs SET next_attempt_at = now() - interval '2 minutes', attempt_count = 49 WHERE event_key = 'broken'"); err != nil {
		t.Fatal(err)
	}
	if ok, err := app.caughtUp(ctx); err != nil || ok {
		t.Fatalf("a runnable job waiting beyond the horizon kept progress: %v %v", ok, err)
	}
	outcome, err = app.processNextJob(ctx, "owner")
	if err != nil || !outcome.deadLettered || outcome.attempt != deadLetterAttempt {
		t.Fatalf("ceiling failure = %+v %v", outcome, err)
	}
	if outcome, err := app.processNextJob(ctx, "owner"); err != nil || !outcome.idle {
		t.Fatalf("a dead-lettered job was claimed again: %+v %v", outcome, err)
	}
	if ok, err := app.caughtUp(ctx); err != nil || !ok {
		t.Fatalf("an empty runnable queue stalled progress: %v %v", ok, err)
	}
	// A vault whose head job keeps failing holds its later jobs behind it.
	// Those jobs are not claimable, so they are not backlog either: the
	// head's failure is reported through Failed, not a stalled observer.
	vault.Vault = "blocked-vault"
	for slot, key := range map[uint64]string{7: "head", 8: "behind"} {
		if _, err := app.store.Enqueue(ctx, app.consumer, key, slot, json.RawMessage(`"not an event"`), []watch.Vault{vault}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.earn_reconciliation_jobs
        SET next_attempt_at = CASE event_key WHEN 'head' THEN now() + interval '5 minutes' ELSE now() - interval '2 minutes' END,
            attempt_count = CASE event_key WHEN 'head' THEN 12 ELSE 0 END
        WHERE event_key IN ('head', 'behind')`); err != nil {
		t.Fatal(err)
	}
	if ok, err := app.caughtUp(ctx); err != nil || !ok {
		t.Fatalf("jobs queued behind a deferred head job stalled progress: %v %v", ok, err)
	}
	if outcome, err := app.processNextJob(ctx, "owner"); err != nil || !outcome.idle {
		t.Fatalf("a job behind its vault's pending head was claimed: %+v %v", outcome, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE loyal_yield.earn_reconciliation_jobs SET next_attempt_at = now() - interval '2 minutes' WHERE event_key = 'head'"); err != nil {
		t.Fatal(err)
	}
	if ok, err := app.caughtUp(ctx); err != nil || ok {
		t.Fatalf("an overdue head job kept progress: %v %v", ok, err)
	}
}
