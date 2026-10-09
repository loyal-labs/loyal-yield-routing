package fleetexec

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mr-tron/base58"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

// A confirmed movement whose reconciliation keeps failing is retried on
// Rust's schedule (twelve one-second attempts, then doubling to a minute)
// instead of on every claim, keeps its custody, and is reported exactly once
// when it reaches sixty attempts (Oct 8: submission 44943 failed silently for
// 14h while its capacity reservation starved the target reserve).
func TestPersistentReconciliationFailureBacksOffAndReportsStall(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	baseline := seedBaseline(t, ctx, pool, fmt.Sprint(time.Now().UnixNano()))
	wire, _ := integrationWire(t, 21)
	id, _, err := store.PersistSignedRoute(ctx, fixturePersistInput(t, ctx, pool, baseline, wire))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions SET submission_state='reconciliation_pending',confirmed_slot=700 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	// The finalized receipt never matches the signed message.
	status := &finalizedStatus{receipt: chain.Receipt{Slot: 700, Wire: []byte("different-wire")}}
	registry := prometheus.NewRegistry()
	worker, err := NewWorker(Config{Cluster: baseline.Cluster, Owner: "owner-stall", LeaseTTL: time.Minute, BatchSize: 8, TickInterval: time.Second, Facts: engine.NewFacts(registry)}, store, &countingChain{}, status, DelegateSigner{})
	if err != nil {
		t.Fatal(err)
	}
	tick := func() {
		t.Helper()
		if err := worker.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		worker.wg.Wait()
	}
	due := func(attempts int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions SET confirmation_attempt_count=$2,confirmation_available_at=clock_timestamp()-interval '1 second' WHERE id=$1`, id, attempts); err != nil {
			t.Fatal(err)
		}
	}
	expect := func(attempts int, delay time.Duration, stalls float64) {
		t.Helper()
		var gotAttempts int
		var delaySeconds float64
		var owner, detail *string
		var state string
		if err := pool.QueryRow(ctx, `SELECT confirmation_attempt_count,EXTRACT(EPOCH FROM confirmation_available_at-last_status_checked_at)::float8,confirmation_lease_owner,error_detail,submission_state FROM loyal_yield.signed_route_submissions WHERE id=$1`, id).Scan(&gotAttempts, &delaySeconds, &owner, &detail, &state); err != nil {
			t.Fatal(err)
		}
		if gotAttempts != attempts || delaySeconds < delay.Seconds()-0.5 || delaySeconds > delay.Seconds()+0.5 || owner != nil || detail == nil || !strings.HasPrefix(*detail, "receipt identity") || state != string(StateReconciliationPending) {
			t.Fatalf("attempt %d: attempts=%d delay=%.3fs (want %s) owner=%v detail=%v state=%s", attempts, gotAttempts, delaySeconds, delay, owner, detail, state)
		}
		if got := failedCount(t, registry, "fleet_reconciliation_stalled"); got != stalls {
			t.Fatalf("attempt %d: stall facts = %v, want %v", attempts, got, stalls)
		}
	}

	tick()
	expect(1, time.Second, 0)
	// Deferred work is not claimed again before its next poll.
	tick()
	expect(1, time.Second, 0)
	due(12)
	tick()
	expect(13, 2*time.Second, 0)
	due(58)
	tick()
	expect(59, time.Minute, 0)
	due(59)
	tick()
	expect(60, time.Minute, 1)
	due(60)
	tick()
	expect(61, time.Minute, 1)

	var reservation string
	if err := pool.QueryRow(ctx, `SELECT reservation_state FROM loyal_yield.target_capacity_reservations WHERE opportunity_id=$1`, baseline.OpportunityID).Scan(&reservation); err != nil {
		t.Fatal(err)
	}
	if reservation != "active" {
		t.Fatalf("failing reconciliation released capacity: %s", reservation)
	}
}

// A claimed opportunity that cannot be admitted (here its fresh evidence is
// unavailable; in production the atomic capacity economics turned
// ineligible) is returned to revalidate with its reason, as Rust's
// revalidate-lane Retry, and the tick completes. The next tick admits another
// vault's opportunity rather than the failed one.
func TestIneligibleOpportunityIsRecordedAndDoesNotFailTheTick(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	cluster := "isolation:" + suffix
	key := ed25519.NewKeyFromSeed(rotateSeed(31, 0))
	signer := base58.Encode(key[32:])
	epochID := seedIsolationEpoch(t, ctx, pool, cluster, suffix)
	_, first := seedRevalidateOpportunity(t, ctx, pool, cluster, epochID, suffix+"-first", signer, 1_000_000)
	_, second := seedRevalidateOpportunity(t, ctx, pool, cluster, epochID, suffix+"-second", signer, 10)

	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"isolation node has no route evidence"}}`))
	}))
	t.Cleanup(node.Close)
	routes, err := fleet.NewStoreFromPool(pool)
	if err != nil {
		t.Fatal(err)
	}
	client, err := chain.New(node.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	revalidator, err := fleet.NewRevalidator(routes, client, fleet.RevalidatorConfig{Owner: "owner-isolation", DelegatedSigner: signer, LeaseTTL: 30 * time.Second, SlotDuration: 400 * time.Millisecond, FusedExecute: true})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewWorker(Config{Cluster: cluster, Owner: "owner-isolation", LeaseTTL: 30 * time.Second, BatchSize: 8, TickInterval: time.Second, Facts: testFacts()}, store, &countingChain{}, &fakeStatus{}, DelegateSigner{FeePayer: key})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.SetFreshRevalidator(revalidator); err != nil {
		t.Fatal(err)
	}
	opportunity := func(id int64) (state string, attempts int, owner, reason *string, later bool) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT opportunity_state,attempt_count,lease_owner,terminal_reason,available_at>clock_timestamp() FROM loyal_yield.rebalance_opportunities WHERE id=$1`, id).Scan(&state, &attempts, &owner, &reason, &later); err != nil {
			t.Fatal(err)
		}
		return
	}

	if err := worker.Tick(ctx); err != nil {
		t.Fatalf("one opportunity's outcome failed the tick: %v", err)
	}
	state, attempts, owner, reason, later := opportunity(first)
	if state != "revalidate" || attempts != 1 || owner != nil || reason == nil || !strings.Contains(*reason, "rpc error -32000") || !later {
		t.Fatalf("failed opportunity not recorded: state=%s attempts=%d owner=%v reason=%v later=%v", state, attempts, owner, reason, later)
	}
	if _, attempts, _, _, _ := opportunity(second); attempts != 0 {
		t.Fatalf("second opportunity claimed out of priority order: attempts=%d", attempts)
	}

	if err := worker.Tick(ctx); err != nil {
		t.Fatalf("second tick failed: %v", err)
	}
	if _, attempts, _, _, _ := opportunity(second); attempts != 1 {
		t.Fatalf("next tick did not reach the other vault's opportunity: attempts=%d", attempts)
	}
	if _, attempts, _, _, _ := opportunity(first); attempts != 1 {
		t.Fatalf("deferred opportunity was claimed again before its retry time: attempts=%d", attempts)
	}
}

// Rust's claim skipped every vault whose rebalance decision is still moving
// money (planned through confirming), whatever the opportunity's priority.
func TestVaultWithActiveDecisionIsNotClaimed(t *testing.T) {
	_, pool := integrationStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	cluster := "active-decision:" + suffix
	signer := base58.Encode(ed25519.NewKeyFromSeed(rotateSeed(41, 0))[32:])
	epochID := seedIsolationEpoch(t, ctx, pool, cluster, suffix)
	busyVault, busy := seedRevalidateOpportunity(t, ctx, pool, cluster, epochID, suffix+"-busy", signer, 1_000_000)
	_, idle := seedRevalidateOpportunity(t, ctx, pool, cluster, epochID, suffix+"-idle", signer, 10)
	var decisionID int64
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.rebalance_decisions(vault_id,decision_reason,idempotency_key,status) VALUES($1,'target_supply_apy_exceeds_source',$2,'submitted') RETURNING id`, busyVault, "decision:"+suffix).Scan(&decisionID); err != nil {
		t.Fatal(err)
	}
	routes, err := fleet.NewStoreFromPool(pool)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := routes.ClaimRevalidation(ctx, cluster, "owner-active-decision", time.Minute, true, false, signer)
	if err != nil || lease == nil || lease.OpportunityID != idle {
		t.Fatalf("claimed %+v (err %v), want only the idle vault's opportunity %d", lease, err, idle)
	}
	if lease, err := routes.ClaimRevalidation(ctx, cluster, "owner-active-decision", time.Minute, true, false, signer); err != nil || lease != nil {
		t.Fatalf("vault with an active decision was claimed: %+v %v", lease, err)
	}
	// Once the decision settles the vault's opportunity is claimable again.
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.rebalance_decisions SET status='confirmed' WHERE id=$1`, decisionID); err != nil {
		t.Fatal(err)
	}
	if lease, err := routes.ClaimRevalidation(ctx, cluster, "owner-active-decision", time.Minute, true, false, signer); err != nil || lease == nil || lease.OpportunityID != busy {
		t.Fatalf("settled vault was not claimed: %+v %v", lease, err)
	}
}

// Rust's claim only took a submission whose retained conflict set still
// matched; one submission missing a retained row must not fail the claim of
// every other submission (the whole executor tick returned before landing).
func TestMismatchedConflictSetDoesNotBlockOtherClaims(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	broken := seedBaseline(t, ctx, pool, suffix+"-broken")
	healthy := seedBaseline(t, ctx, pool, suffix+"-healthy", broken.Cluster)
	brokenWire, _ := integrationWire(t, 51)
	brokenID, _, err := store.PersistSignedRoute(ctx, fixturePersistInput(t, ctx, pool, broken, brokenWire))
	if err != nil {
		t.Fatal(err)
	}
	healthyWire, _ := integrationWire(t, 51)
	healthyID, _, err := store.PersistSignedRoute(ctx, fixturePersistInput(t, ctx, pool, healthy, healthyWire))
	if err != nil {
		t.Fatal(err)
	}
	if tag, err := pool.Exec(ctx, `DELETE FROM loyal_yield.route_account_conflict_leases WHERE submission_id=$1 AND writable_account_key LIKE 'shared-lane:%'`, brokenID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("remove retained row: %v", err)
	}
	leases, err := store.ClaimRecoveryWork(ctx, broken.Cluster, "owner-conflict", time.Minute, 8)
	if err != nil {
		t.Fatalf("one mismatched submission failed the whole claim: %v", err)
	}
	if len(leases) != 1 || leases[0].Submission.ID != healthyID {
		t.Fatalf("claimed %d leases, want only submission %d", len(leases), healthyID)
	}
	// The mismatched row stays leased to this owner, so it is not reworked
	// before its lease lapses.
	var owner *string
	if err := pool.QueryRow(ctx, `SELECT confirmation_lease_owner FROM loyal_yield.signed_route_submissions WHERE id=$1`, brokenID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner == nil || *owner != "owner-conflict" {
		t.Fatalf("mismatched submission lease owner = %v", owner)
	}
}

func seedIsolationEpoch(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cluster, suffix string) int64 {
	t.Helper()
	var epochID int64
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.optimizer_epochs(cluster,epoch_key,market_slot,observed_at,expires_at,market_state) VALUES($1,$2,1000,clock_timestamp(),clock_timestamp()+interval '1 hour','{}') RETURNING id`, cluster, "epoch:"+suffix).Scan(&epochID); err != nil {
		t.Fatal(err)
	}
	return epochID
}

// seedRevalidateOpportunity is one vault under a finalized same-mint policy
// delegating to signer, with one claimable same-mint opportunity.
func seedRevalidateOpportunity(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cluster string, epochID int64, suffix, signer string, priority int64) (vaultID, opportunityID int64) {
	t.Helper()
	var policyID int64
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_policies(cluster,settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,delegated_signers,threshold,route_modes,stable_mints,kamino_markets,kamino_liquidity_mints,swap_lanes,active,source_commitment,finalized_eligible,last_seen_slot,last_seen_signature) VALUES($1,$2,$3,1,$4,0,$5,ARRAY[$3]::text[],1,ARRAY['same_mint_kamino']::text[],ARRAY['usdc']::text[],ARRAY[$6]::text[],ARRAY['usdc']::text[],'[]',true,'finalized',true,999,$7) RETURNING id`,
		cluster, "settings:"+suffix, signer, "policy:"+suffix, "vault:"+suffix, "market:"+suffix, "signature:"+suffix).Scan(&policyID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.managed_vaults(settings,vault_index,vault_pubkey,active_policy_id,active) VALUES($1,0,$2,$3,true) RETURNING id`, "settings:"+suffix, "vault:"+suffix, policyID).Scan(&vaultID); err != nil {
		t.Fatal(err)
	}
	mint := "usdc:" + suffix
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.rebalance_opportunities(cluster,idempotency_key,vault_id,optimizer_epoch_id,source_reserve,target_reserve,liquidity_mint,source_liquidity_mint,target_liquidity_mint,amount_raw,principal_usd_micros,source_apy_bps,target_apy_bps,estimated_edge_bps,annual_yield_gain_usd_micros,expected_net_gain_usd_micros,economic_priority,scheduler_priority_anchor,priority_version,opportunity_state,expires_at,execution_plan) VALUES($1,$2,$3,$4,$5,$6,$7,$7,$7,1000000,1000000,100,900,800,1000,900,$8,1,'v1','revalidate',clock_timestamp()+interval '1 hour','{"route_kind":"same_mint","source_kind":"reserve_position"}') RETURNING id`,
		cluster, "opportunity:"+suffix, vaultID, epochID, "source:"+suffix, "target:"+suffix, mint, priority).Scan(&opportunityID); err != nil {
		t.Fatal(err)
	}
	return vaultID, opportunityID
}

func failedCount(t *testing.T, registry *prometheus.Registry, code string) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "loyal_family_failed_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["family"] == string(engine.FamilyFleet) && labels["code"] == code {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}
