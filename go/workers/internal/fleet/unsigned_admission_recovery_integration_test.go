package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
)

type unsignedAdmissionFixture struct {
	cluster string
	lease   RevalidationLease
	commit  RevalidationCommit
}

func unsignedRecoveryStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("FLEET_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("FLEET_TEST_DATABASE_URL is not set")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Path != "/fleet" {
		t.Fatal("unsigned admission tests require the disposable loopback fleet database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	store, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return store, ctx
}

// This is a registered-schema prepublication fixture, not ABI or signed-wire
// validation. It exercises the real atomic fused admission store boundary.
func seedUnsignedAdmission(t *testing.T, ctx context.Context, store *Store, cluster string) unsignedAdmissionFixture {
	t.Helper()
	suffix := fmt.Sprint(time.Now().UnixNano())
	if cluster == "" {
		cluster = "unsigned-recovery-" + suffix
	}
	source := ReserveIdentity{Address: testIdentity(43), Market: testIdentity(42), Mint: USDCMint}
	target := ReserveIdentity{Address: testIdentity(44), Market: source.Market, Mint: USDCMint}
	vaultID := seedWorkerVault(t, ctx, store, suffix, source.Market, source.Address)
	// Retain financial/audit evidence, but do not leave an eligible vault for
	// another test's unscoped fleet scan. No guards or triggers are bypassed.
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := store.pool.Exec(cleanup, `UPDATE loyal_yield.managed_vaults SET active=false WHERE id=$1`, vaultID); err != nil {
			t.Error(err)
		}
	})
	position, err := store.LoadVaultPosition(ctx, cluster, vaultID, source, target)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := MarketSnapshot{Slot: 1000, ObservedAt: time.Now().UTC(), Hash: "unsigned-" + suffix, Reserves: map[string]ReserveState{
		source.Address: {ReserveIdentity: source, Slot: 1000, LastUpdateSlot: 1000, SupplyAPYBPS: 100, TotalSupplyUSDMicros: 1_000_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: strings.Repeat("a", 64)},
		target.Address: {ReserveIdentity: target, Slot: 1000, LastUpdateSlot: 1000, SupplyAPYBPS: 900, TotalSupplyUSDMicros: 1_000_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: strings.Repeat("b", 64)},
	}}
	epoch := testImmutableMarketEpoch(t, snapshot, source, target)
	published, err := store.Publish(ctx, cluster, epoch, position, Plan(snapshot, position, source.Address, target.Address))
	if err != nil || !published.Inserted {
		t.Fatalf("publish: %+v %v", published, err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO loyal_yield.target_capacity_frontiers(cluster,target_reserve,liquidity_mint,observed_supply_usd_micros,observed_slot,maximum_inflight_usd_micros,telemetry_version)
VALUES($1,$2,$3,1000000000000000,1000,20000000000000,1) ON CONFLICT DO NOTHING`, cluster, target.Address, USDCMint); err != nil {
		t.Fatal(err)
	}
	lease, err := store.ClaimRevalidation(ctx, cluster, "unsigned-owner-"+suffix, time.Minute, false, false)
	if err != nil || lease == nil || lease.OpportunityID != published.OpportunityID {
		t.Fatalf("claim: %+v %v", lease, err)
	}
	wireHash := strings.Repeat("f", 64)
	prep := &RoutePreparation{RouteFingerprint: strings.Repeat("1", 64), RequirementsFingerprint: strings.Repeat("2", 64), ExecutionPlan: json.RawMessage(`{"message_base64":"AQ==","unsigned_wire_base64":"Ag==","simulation":{"succeeded":true}}`), Transaction: PreparedTransaction{Message: []byte{1}, UnsignedWire: []byte{2}, WireSHA256: wireHash, PacketBytes: 1, FeeLamports: 1, ComputeLimit: 1}, Simulation: SimulationEvidence{Slot: 1001, Succeeded: true, UnitsConsumed: 1, WireSHA256: wireHash}}
	if err := preserveCanonicalPlan(lease.ExecutionPlan, prep, "prepared_transaction"); err != nil {
		t.Fatal(err)
	}
	commit := RevalidationCommit{Disposition: "fused_execute", Preparation: prep, ConflictKeys: []string{"vault:" + position.VaultPubkey, "source-reserve:" + source.Address}, ExpectedEpochFingerprint: epoch.Fingerprint, ExpectedOpportunityKey: lease.IdempotencyKey, FreshEconomics: true, ObservedSourceAPYBPS: 100, ObservedTargetAPYBPS: 900, TargetObservedSupplyUSDMicros: 1_000_000_000_000_000, TargetObservedSlot: 1000}
	if err := store.commitRevalidation(ctx, *lease, commit, nil); err != nil {
		t.Fatal(err)
	}
	return unsignedAdmissionFixture{cluster: cluster, lease: *lease, commit: commit}
}

func expireUnsignedLease(t *testing.T, ctx context.Context, store *Store, fixture unsignedAdmissionFixture) {
	t.Helper()
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities
SET created_at=clock_timestamp()-interval '2 minutes',lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, fixture.lease.OpportunityID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.route_account_conflict_leases
SET created_at=clock_timestamp()-interval '2 minutes',expires_at=clock_timestamp()-interval '1 second'
WHERE opportunity_id=$1 AND submission_id IS NULL`, fixture.lease.OpportunityID); err != nil {
		t.Fatal(err)
	}
}

func TestUnsignedAdmissionRecoveryIntegrationTransitions(t *testing.T) {
	store, ctx := unsignedRecoveryStore(t)
	for _, scenario := range []string{"current", "insufficient_lifetime", "already_stale", "live_lease"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := seedUnsignedAdmission(t, ctx, store, "")
			if scenario != "live_lease" {
				expireUnsignedLease(t, ctx, store, fixture)
			}
			if scenario == "insufficient_lifetime" {
				if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET expires_at=clock_timestamp()+interval '30 seconds' WHERE id=$1`, fixture.lease.OpportunityID); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "already_stale" {
				if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET opportunity_state='stale',lease_kind=NULL,lease_owner=NULL,lease_expires_at=NULL,terminal_reason='prior_sweep' WHERE id=$1`, fixture.lease.OpportunityID); err != nil {
					t.Fatal(err)
				}
			}
			if count, err := store.RecoverUnsignedExecutionAdmissions(ctx, fixture.cluster+"-other", 1); err != nil || count != 0 {
				t.Fatalf("cluster fence: %d %v", count, err)
			}
			wantCount := int64(1)
			if scenario == "live_lease" {
				wantCount = 0
			}
			if count, err := store.RecoverUnsignedExecutionAdmissions(ctx, fixture.cluster, 1); err != nil || count != wantCount {
				t.Fatalf("recovery: %d want %d: %v", count, wantCount, err)
			}
			var state, reservationState, reason string
			var token, version int64
			var leaseFieldsNull bool
			if err := store.pool.QueryRow(ctx, `SELECT o.opportunity_state,o.fencing_token,o.lease_owner IS NULL AND o.lease_kind IS NULL AND o.lease_expires_at IS NULL,r.reservation_state,r.state_version,COALESCE(r.release_reason,'') FROM loyal_yield.rebalance_opportunities o JOIN loyal_yield.target_capacity_reservations r ON r.opportunity_id=o.id WHERE o.id=$1`, fixture.lease.OpportunityID).Scan(&state, &token, &leaseFieldsNull, &reservationState, &version, &reason); err != nil {
				t.Fatal(err)
			}
			if scenario == "live_lease" {
				if state != "leased" || token != fixture.lease.FencingToken || leaseFieldsNull || reservationState != "active" || version != 1 || reason != "" {
					t.Fatalf("live ownership changed: %s %d %t %s %d %s", state, token, leaseFieldsNull, reservationState, version, reason)
				}
				return
			}
			wantState := "stale"
			if scenario == "current" {
				wantState = "revalidate"
			}
			if state != wantState || token != fixture.lease.FencingToken+1 || !leaseFieldsNull || reservationState != "released" || version != 2 || reason != "unsigned_execution_lease_expired" {
				t.Fatalf("recovery incomplete: %s %d %t %s %d %s", state, token, leaseFieldsNull, reservationState, version, reason)
			}
			var conflicts int
			if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.route_account_conflict_leases WHERE opportunity_id=$1`, fixture.lease.OpportunityID).Scan(&conflicts); err != nil || conflicts != 0 {
				t.Fatalf("unsigned conflict ownership retained: %d %v", conflicts, err)
			}
			if err := store.commitRevalidation(ctx, fixture.lease, fixture.commit, nil); err == nil {
				t.Fatal("old admission owner crossed recovery fence")
			}
			if count, err := store.RecoverUnsignedExecutionAdmissions(ctx, fixture.cluster, 1); err != nil || count != 0 {
				t.Fatalf("duplicate release: %d %v", count, err)
			}
		})
	}
}

func TestUnsignedAdmissionRecoveryIntegrationLocksAndCancellation(t *testing.T) {
	store, ctx := unsignedRecoveryStore(t)
	fixture := seedUnsignedAdmission(t, ctx, store, "")
	expireUnsignedLease(t, ctx, store, fixture)
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SELECT id FROM loyal_yield.rebalance_opportunities WHERE id=$1 FOR UPDATE`, fixture.lease.OpportunityID); err != nil {
		t.Fatal(err)
	}
	if count, err := store.RecoverUnsignedExecutionAdmissions(ctx, fixture.cluster, 1); err != nil || count != 0 {
		t.Fatalf("opportunity SKIP LOCKED: %d %v", count, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err = store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SELECT observed_slot FROM loyal_yield.target_capacity_frontiers WHERE cluster=$1 FOR UPDATE`, fixture.cluster); err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if count, err := store.RecoverUnsignedExecutionAdmissions(blocked, fixture.cluster, 1); err == nil || count != 0 {
		t.Fatalf("blocked frontier ignored cancellation: %d %v", count, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// A pgx deadline can close a connection while PostgreSQL is still handling
	// its cancellation. A bounded row-lock barrier proves server rollback has
	// finished; an immediate SKIP LOCKED probe alone cannot prove a leaked lock.
	barrierCtx, barrierCancel := context.WithTimeout(ctx, time.Second)
	defer barrierCancel()
	barrier, err := store.pool.Begin(barrierCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Rollback(context.Background())
	if _, err := barrier.Exec(barrierCtx, `SELECT id FROM loyal_yield.rebalance_opportunities WHERE id=$1 FOR UPDATE`, fixture.lease.OpportunityID); err != nil {
		t.Fatalf("canceled recovery retained a server transaction: %v", err)
	}
	if err := barrier.Rollback(barrierCtx); err != nil {
		t.Fatal(err)
	}
	var state string
	var version int64
	if err := store.pool.QueryRow(ctx, `SELECT reservation_state,state_version FROM loyal_yield.target_capacity_reservations WHERE opportunity_id=$1`, fixture.lease.OpportunityID).Scan(&state, &version); err != nil || state != "active" || version != 1 {
		t.Fatalf("canceled recovery leaked effects: %s %d %v", state, version, err)
	}
	if count, err := store.RecoverUnsignedExecutionAdmissions(ctx, fixture.cluster, 1); err != nil || count != 1 {
		t.Fatalf("canceled transaction retained opportunity lock: %d %v", count, err)
	}
}

func TestUnsignedAdmissionRecoveryIntegrationBoundedBatch(t *testing.T) {
	store, ctx := unsignedRecoveryStore(t)
	first := seedUnsignedAdmission(t, ctx, store, "")
	expireUnsignedLease(t, ctx, store, first)
	// Leave the first candidate locked while the second vault is admitted.
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SELECT id FROM loyal_yield.rebalance_opportunities WHERE id=$1 FOR UPDATE`, first.lease.OpportunityID); err != nil {
		t.Fatal(err)
	}
	second := seedUnsignedAdmission(t, ctx, store, first.cluster)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	expireUnsignedLease(t, ctx, store, second)
	for batch := 0; batch < 2; batch++ {
		if count, err := store.RecoverUnsignedExecutionAdmissions(ctx, first.cluster, 1); err != nil || count != 1 {
			t.Fatalf("bounded recovery %d: %d %v", batch, count, err)
		}
		var released int
		if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.target_capacity_reservations WHERE cluster=$1 AND reservation_state='released'`, first.cluster).Scan(&released); err != nil || released != batch+1 {
			t.Fatalf("batch exceeded limit: %d %v", released, err)
		}
	}
}

func TestUnsignedAdmissionRecoveryIntegrationReadmitsSameOpportunity(t *testing.T) {
	store, ctx := unsignedRecoveryStore(t)
	fixture := seedUnsignedAdmission(t, ctx, store, "")
	var reservationID, oldGeneration int64
	if err := store.pool.QueryRow(ctx, `SELECT id,reservation_generation FROM loyal_yield.target_capacity_reservations WHERE opportunity_id=$1`, fixture.lease.OpportunityID).Scan(&reservationID, &oldGeneration); err != nil {
		t.Fatal(err)
	}
	expireUnsignedLease(t, ctx, store, fixture)
	if count, err := store.RecoverUnsignedExecutionAdmissions(ctx, fixture.cluster, 1); err != nil || count != 1 {
		t.Fatalf("recover before re-admission: %d %v", count, err)
	}
	lease, err := store.ClaimRevalidation(ctx, fixture.cluster, "replacement-executor", time.Minute, false, false)
	if err != nil || lease == nil || lease.OpportunityID != fixture.lease.OpportunityID || lease.FencingToken <= fixture.lease.FencingToken {
		t.Fatalf("replacement claim: %+v %v", lease, err)
	}
	if err := store.commitRevalidation(ctx, *lease, fixture.commit, nil); err != nil {
		t.Fatalf("released unsigned reservation prevented re-admission: %v", err)
	}
	var newID, generation, fence, version int64
	var state string
	var releaseCleared bool
	if err := store.pool.QueryRow(ctx, `SELECT id,reservation_generation,reservation_fencing_token,state_version,reservation_state,released_at IS NULL AND release_reason IS NULL FROM loyal_yield.target_capacity_reservations WHERE opportunity_id=$1`, fixture.lease.OpportunityID).Scan(&newID, &generation, &fence, &version, &state, &releaseCleared); err != nil {
		t.Fatal(err)
	}
	if newID != reservationID || generation <= oldGeneration || fence != lease.FencingToken || version != 3 || state != "active" || !releaseCleared {
		t.Fatalf("new capacity fence incomplete: id=%d generation=%d fence=%d version=%d state=%s cleared=%t", newID, generation, fence, version, state, releaseCleared)
	}
	if err := store.CheckRevalidationLease(ctx, fixture.lease); err == nil {
		t.Fatal("old executor lease survived replacement admission")
	}
	if err := store.commitRevalidation(ctx, fixture.lease, fixture.commit, nil); err == nil {
		t.Fatal("old admission advanced replacement capacity ownership")
	}
	var unchanged int64
	if err := store.pool.QueryRow(ctx, `SELECT reservation_fencing_token FROM loyal_yield.target_capacity_reservations WHERE opportunity_id=$1`, fixture.lease.OpportunityID).Scan(&unchanged); err != nil || unchanged != fence {
		t.Fatalf("stale executor changed replacement reservation: %d %v", unchanged, err)
	}
}

// Publish a fully reciprocal schema fixture with normal decision, opportunity,
// capacity and signed-journal guards. This proves ownership protection; byte
// validity and chain outcomes remain the executor family's responsibility.
func linkSignedAdmission(t *testing.T, ctx context.Context, store *Store, fixture unsignedAdmissionFixture) int64 {
	t.Helper()
	payer := fixture.lease.DelegatedSigners[0]
	addressBytes := sha256.Sum256([]byte(fixture.cluster))
	table := solana.PublicKeyFromBytes(addressBytes[:]).String()
	seedConnectedLookupTable(t, ctx, store, fixture.cluster, payer, fixture.lease.VaultID, table, []string{fixture.lease.TargetReserve}, "shared_market", "shared_market")
	var tableID int64
	if err := store.pool.QueryRow(ctx, `SELECT id FROM loyal_yield.route_lookup_tables WHERE table_address=$1`, table).Scan(&tableID); err != nil {
		t.Fatal(err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var submissionID, decisionID int64
	if err := tx.QueryRow(ctx, `INSERT INTO loyal_yield.signed_route_submissions(cluster,semantic_key,opportunity_id,signed_transaction,signed_transaction_hash,message_hash,transaction_signature,recent_blockhash,last_valid_block_height,source_snapshot_id,optimizer_epoch_id,alt_requirements_fingerprint,alt_selection_fingerprint,alt_mutation_epochs,fee_payer,compiled_fee_lamports,writable_account_keys,conflict_account_keys,executor_owner,executor_fencing_token)
SELECT cluster,$2,id,'\x01','schema-fixture-wire','schema-fixture-message',$2,'schema-fixture-blockhash',1000,source_snapshot_id,optimizer_epoch_id,requirements_fingerprint,'schema-fixture-selection',jsonb_build_object('tables',jsonb_build_array(jsonb_build_object('tableId',$4::bigint,'mutationEpoch',0))),$3,5000,ARRAY[$3,'schema-fixture-vault'],ARRAY[$3,'schema-fixture-vault'],lease_owner,fencing_token
FROM loyal_yield.rebalance_opportunities WHERE id=$1 RETURNING id`, fixture.lease.OpportunityID, fixture.cluster, payer, tableID).Scan(&submissionID); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO loyal_yield.rebalance_decisions(vault_id,source_snapshot_id,source_reserve,target_reserve,liquidity_mint,amount_raw,source_apy_bps,target_apy_bps,estimated_edge_bps,estimated_cost_lamports,decision_reason,execution_plan,idempotency_key)
SELECT vault_id,source_snapshot_id,source_reserve,target_reserve,liquidity_mint,amount_raw,source_apy_bps,target_apy_bps,estimated_edge_bps,estimated_cost_lamports,'target_supply_apy_exceeds_source',execution_plan,$2
FROM loyal_yield.rebalance_opportunities WHERE id=$1 RETURNING id`, fixture.lease.OpportunityID, fixture.cluster).Scan(&decisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE loyal_yield.target_capacity_reservations SET decision_id=$2,signed_submission_id=$3 WHERE opportunity_id=$1`, fixture.lease.OpportunityID, decisionID, submissionID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE loyal_yield.route_account_conflict_leases SET submission_id=$2 WHERE opportunity_id=$1`, fixture.lease.OpportunityID, submissionID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return submissionID
}

func TestUnsignedAdmissionRecoveryIntegrationPreservesSignedOwnership(t *testing.T) {
	store, ctx := unsignedRecoveryStore(t)
	for _, state := range []string{"signed", "effect_ambiguous", "failed", "expired", "reconciled"} {
		t.Run(state, func(t *testing.T) {
			fixture := seedUnsignedAdmission(t, ctx, store, "")
			submissionID := linkSignedAdmission(t, ctx, store, fixture)
			if state == "reconciled" {
				if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.rebalance_decisions SET status='confirmed' WHERE id=(SELECT decision_id FROM loyal_yield.signed_route_submissions WHERE id=$1)`, submissionID); err != nil {
					t.Fatal(err)
				}
				if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions SET submission_state='reconciled',confirmed_slot=1001,reconciled_slot=1002 WHERE id=$1`, submissionID); err != nil {
					t.Fatal(err)
				}
			} else if state != "signed" {
				if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions SET submission_state=$2 WHERE id=$1`, submissionID, state); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET expires_at=clock_timestamp()-interval '1 second',created_at=clock_timestamp()-interval '2 minutes',available_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, fixture.lease.OpportunityID); err != nil {
				t.Fatal(err)
			}
			read := func() string {
				t.Helper()
				var evidence string
				if err := store.pool.QueryRow(ctx, `SELECT jsonb_build_array(o.opportunity_state,o.fencing_token,o.decision_id,r.reservation_state,r.state_version,r.release_reason,r.signed_submission_id,s.submission_state,s.signed_transaction,s.transaction_signature,(SELECT count(*) FROM loyal_yield.route_account_conflict_leases c WHERE c.opportunity_id=o.id))::text FROM loyal_yield.rebalance_opportunities o JOIN loyal_yield.target_capacity_reservations r ON r.opportunity_id=o.id JOIN loyal_yield.signed_route_submissions s ON s.id=r.signed_submission_id WHERE o.id=$1`, fixture.lease.OpportunityID).Scan(&evidence); err != nil {
					t.Fatal(err)
				}
				return evidence
			}
			before := read()
			if count, err := store.RecoverUnsignedExecutionAdmissions(ctx, fixture.cluster, 10); err != nil || count != 0 {
				t.Fatalf("signed history reclaimed: %d %v", count, err)
			}
			if after := read(); after != before {
				t.Fatalf("signed evidence/capacity changed: before=%s after=%s", before, after)
			}
		})
	}
}
