package fleet

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestExpiryIntegrationRecoveryAndOwnership(t *testing.T) {
	databaseURL := os.Getenv("FLEET_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("FLEET_TEST_DATABASE_URL is not set")
	}
	u, err := url.Parse(databaseURL)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Path != "/fleet" {
		t.Fatal("expiry fault injection requires the disposable loopback fleet database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := OpenStore(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, state := range []string{"waiting_alt", "revalidate", "ready", "leased"} {
		t.Run(state, func(t *testing.T) {
			suffix := fmt.Sprint(time.Now().UnixNano())
			cluster := "expiry-" + suffix
			source := ReserveIdentity{Address: testIdentity(3), Market: testIdentity(40), Mint: USDCMint}
			target := ReserveIdentity{Address: testIdentity(80), Market: source.Market, Mint: USDCMint}
			vaultID := seedWorkerVault(t, ctx, store, suffix, source.Market, source.Address)
			snapshot := MarketSnapshot{Slot: 1000, ObservedAt: time.Now().UTC(), Hash: strings.Repeat("a", 64), Reserves: map[string]ReserveState{
				source.Address: {ReserveIdentity: source, Slot: 1000, LastUpdateSlot: 1000, SupplyAPYBPS: 100, TotalSupplyUSDMicros: 2_000_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: strings.Repeat("a", 64)},
				target.Address: {ReserveIdentity: target, Slot: 1000, LastUpdateSlot: 1000, SupplyAPYBPS: 900, TotalSupplyUSDMicros: 2_000_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: strings.Repeat("b", 64)},
			}}
			epoch := testImmutableMarketEpoch(t, snapshot, source, target)
			position, err := store.LoadVaultPosition(ctx, cluster, vaultID, source, target)
			if err != nil {
				t.Fatal(err)
			}
			decision := Plan(snapshot, position, source.Address, target.Address)
			p, err := store.Publish(ctx, cluster, epoch, position, decision)
			if err != nil || !p.Inserted {
				t.Fatalf("publish: %+v %v", p, err)
			}
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := store.pool.Exec(ctx, sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			// Leave an expired route whose owner may still be working. Live ownership
			// must win over expiry, including an unresolved persisted signed submission.
			exec(`UPDATE loyal_yield.rebalance_opportunities SET opportunity_state=$2,route_fingerprint='route',requirements_fingerprint='requirements',lease_kind=CASE WHEN $2='leased' THEN 'revalidate' END,lease_owner=CASE WHEN $2='leased' THEN 'test-owner' END,lease_expires_at=CASE WHEN $2='leased' THEN now()+interval '1 minute' END WHERE id=$1`, p.OpportunityID, state)
			// Advance the fixture's age after entering the state, retaining the
			// production commit-time lifetime trigger during the transition.
			exec(`UPDATE loyal_yield.rebalance_opportunities SET created_at=now()-interval '10 minutes',available_at=now()-interval '9 minutes',expires_at=now()-interval '1 minute' WHERE id=$1`, p.OpportunityID)
			sweep := func(want int64) {
				t.Helper()
				n, e := store.SweepExpiredOpportunities(ctx, cluster, 1)
				if e != nil || n != want {
					t.Fatalf("sweep=%d want=%d err=%v", n, want, e)
				}
			}
			if state == "leased" {
				sweep(0)
				exec(`UPDATE loyal_yield.rebalance_opportunities SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, p.OpportunityID)
			}
			// Fault-inject a legacy/partial signed handoff without its decision.
			// Normal production triggers forbid this state. Only fixture writes
			// bypass triggers, transaction-locally; the real sweeper runs normally.
			// This is a defensive ownership test, NOT a lifecycle execution test.
			inject := func(sql string, args ...any) {
				t.Helper()
				tx, err := store.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				if _, err = tx.Exec(ctx, `SET LOCAL session_replication_role=replica`); err != nil {
					t.Fatal(err)
				}
				if _, err = tx.Exec(ctx, sql, args...); err != nil {
					t.Fatal(err)
				}
				if err = tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				var role string
				if err = store.pool.QueryRow(ctx, `SHOW session_replication_role`).Scan(&role); err != nil || role != "origin" {
					t.Fatalf("fixture trigger bypass leaked: %s %v", role, err)
				}
			}
			inject(`INSERT INTO loyal_yield.signed_route_submissions(cluster,semantic_key,opportunity_id,signed_transaction,signed_transaction_hash,message_hash,transaction_signature,recent_blockhash,last_valid_block_height,optimizer_epoch_id,alt_requirements_fingerprint,alt_selection_fingerprint,alt_mutation_epochs,fee_payer,compiled_fee_lamports,writable_account_keys,conflict_account_keys,executor_owner,executor_fencing_token,submission_state) VALUES($1,$1,$2,'\x01','hash','message',$1,'blockhash',1000,$3,'requirements','selection','{}','payer',5000,ARRAY['payer','vault'],ARRAY['payer','vault'],'owner',1,'signed')`, cluster, p.OpportunityID, p.EpochID)
			var submissionID int64
			if err = store.pool.QueryRow(ctx, `SELECT id FROM loyal_yield.signed_route_submissions WHERE semantic_key=$1`, cluster).Scan(&submissionID); err != nil {
				t.Fatal(err)
			}
			exec(`INSERT INTO loyal_yield.route_account_conflict_leases(cluster,writable_account_key,opportunity_id,lease_owner,fencing_token,expires_at,submission_id) VALUES($1,'signed-key',$2,'owner',1,now()+interval '1 minute',$3),($1,'unstarted-key',$2,'owner',1,now()+interval '1 minute',NULL)`, cluster, p.OpportunityID, submissionID)
			for _, signedState := range []string{"signed", "submitted", "confirmed", "reconciliation_pending", "expiry_check_pending", "effect_ambiguous"} {
				inject(`UPDATE loyal_yield.signed_route_submissions SET submission_state=$2 WHERE id=$1`, submissionID, signedState)
				sweep(0)
			}
			inject(`UPDATE loyal_yield.signed_route_submissions SET submission_state='failed' WHERE id=$1`, submissionID)
			// SKIP LOCKED must not wait on a concurrent owner, nor mutate another cluster.
			tx, err := store.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(ctx, `SELECT id FROM loyal_yield.rebalance_opportunities WHERE id=$1 FOR UPDATE`, p.OpportunityID); err != nil {
				t.Fatal(err)
			}
			sweep(0)
			if err = tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if n, err := store.SweepExpiredOpportunities(ctx, cluster+"-other", 1); err != nil || n != 0 {
				t.Fatalf("cluster fence: %d %v", n, err)
			}
			sweep(1)
			sweep(0)
			var gotState, reason string
			var conflicts int
			if err = store.pool.QueryRow(ctx, `SELECT opportunity_state,terminal_reason FROM loyal_yield.rebalance_opportunities WHERE id=$1`, p.OpportunityID).Scan(&gotState, &reason); err != nil || gotState != "stale" || reason != "optimizer_epoch_expired" {
				t.Fatalf("expiry state: %s %s %v", gotState, reason, err)
			}
			if err = store.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.route_account_conflict_leases WHERE opportunity_id=$1 AND submission_id IS NOT NULL`, p.OpportunityID).Scan(&conflicts); err != nil || conflicts != 1 {
				t.Fatalf("signed conflict ownership lost: %d %v", conflicts, err)
			}
			if err = store.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.route_account_conflict_leases WHERE opportunity_id=$1 AND submission_id IS NULL`, p.OpportunityID).Scan(&conflicts); err != nil || conflicts != 0 {
				t.Fatalf("unstarted conflict not released: %d %v", conflicts, err)
			}
			position, err = store.LoadVaultPosition(ctx, cluster, vaultID, source, target)
			if err != nil || position.BlockedReason != "" {
				t.Fatalf("vault remains blocked: %+v %v", position, err)
			}
			// Rediscover in a new immutable epoch, as happens after real market expiry.
			snapshot.ObservedAt = snapshot.ObservedAt.Add(time.Second)
			for k, v := range snapshot.Reserves {
				v.Slot++
				snapshot.Reserves[k] = v
			}
			snapshot.Slot++
			nextEpoch := testImmutableMarketEpoch(t, snapshot, source, target)
			next, err := store.Publish(ctx, cluster, nextEpoch, position, Plan(snapshot, position, source.Address, target.Address))
			if err != nil || !next.Inserted || next.OpportunityID == p.OpportunityID {
				t.Fatalf("fresh epoch did not recover vault: %+v %v", next, err)
			}
		})
	}
}
