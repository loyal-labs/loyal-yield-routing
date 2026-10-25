package autodeposit

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earncatalog"
)

func TestControlSnapshotProvesActualArtifacts(t *testing.T) {
	ctx := context.Background()
	f, target, builder := controlFixture(t)
	accounts := installControlSnapshot(t, f, builder, uint64(f.MaxAmountPerPeriod))
	observation, err := builder.ObserveControl(ctx, target, 200)
	if err != nil || observation.status() != "active" || observation.WalletBalanceRaw != 9_000_000 {
		t.Fatalf("actual artifacts observation=%+v err=%v", observation, err)
	}
	if _, err = builder.ObserveControl(ctx, target, 201); err == nil {
		t.Fatal("RPC below minimum slot accepted")
	}
	accounts[f.WalletATA].Data[76] ^= 1
	observation, err = builder.ObserveControl(ctx, target, 200)
	if err != nil || observation.status() != "inconsistent" {
		t.Fatalf("foreign token delegate observation=%+v err=%v", observation, err)
	}
	accounts[f.WalletATA].Data[76] ^= 1
	// A policy whose constraints no longer allow the pull is observed, not
	// retried forever as an unprovable target.
	subscriptions := mustKey(earncatalog.SubscriptionsProgram)
	at := bytes.Index(accounts[f.Policy].Data, subscriptions[:])
	if at < 0 {
		t.Fatal("fixture policy lacks the subscriptions program constraint")
	}
	accounts[f.Policy].Data[at] ^= 1
	observation, err = builder.ObserveControl(ctx, target, 200)
	if err != nil || observation.PolicyValid || observation.status() != "inconsistent" {
		t.Fatalf("policy without the pull observation=%+v err=%v", observation, err)
	}
	accounts[f.Policy].Data[at] ^= 1
	policy := accounts[f.Policy]
	policy.Owner = systemProgramZero
	accounts[f.Policy] = policy
	if _, err = builder.ObserveControl(ctx, target, 200); err == nil {
		t.Fatal("foreign policy activated target")
	}
}

func seedControlTarget(t *testing.T, s *Store, suffix string) ControlTarget {
	t.Helper()
	seeded := seedIntegrationTarget(t, s, suffix)
	_, err := s.pool.Exec(context.Background(), `UPDATE loyal_yield.balance_sweep_targets SET subscription_authority='test-authority',recurring_delegation='test-delegation',recurring_delegation_nonce=7,setup_generation=3,bootstrap_generation=NULL WHERE id=$1`, seeded.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.LoadControlTarget(context.Background(), seeded.TargetID)
	if err != nil || target == nil {
		t.Fatalf("load control target: %v %v", target, err)
	}
	return *target
}

func claimControlRequest(t *testing.T, s *Store, targetID, slot int64, owner string) ReconciliationRequest {
	t.Helper()
	ctx := context.Background()
	enqueueReconciliationRequest(t, s, targetID, slot)
	r, err := s.ClaimAutodepositReconciliationRequest(ctx, owner, 120)
	if err != nil || r == nil {
		t.Fatalf("claim: %v %v", r, err)
	}
	return *r
}

func TestControlBootstrapGenerationAndReadiness(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	target := seedControlTarget(t, s, "control-bootstrap")
	start := time.Now().Add(3 * time.Hour).Unix()
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET start_timestamp=$2 WHERE id=$1`, target.TargetID, start); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadControlTarget(ctx, target.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	target = *loaded
	o := ControlObservation{Target: target, ObservedSlot: 990001, PolicyExists: true, DelegationExists: true, PolicyValid: true, AuthorityValid: true, DelegationValid: true, TokenDelegateValid: true, WalletBalanceRaw: 9_000_000, WalletAccountDataSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	r := claimControlRequest(t, s, target.TargetID, 990000, "control-owner")
	// New asks raised under a running lease survive application.
	enqueueReconciliationRequest(t, s, target.TargetID, 990002)
	if err := s.ApplyControlObservation(ctx, r, "control-owner", o); err != nil {
		t.Fatal(err)
	}
	var amount, eventID, generation, requested, processed int64
	var eligible time.Time
	var commitment string
	if err := s.pool.QueryRow(ctx, `SELECT lot.original_amount_raw,lot.source_event_id,lot.eligible_after,target.bootstrap_generation,event.source_commitment FROM loyal_yield.balance_sweep_surplus_lots lot JOIN loyal_yield.balance_sweep_targets target ON target.id=lot.target_id JOIN loyal_yield.balance_sweep_wallet_balance_events event ON event.event_id=lot.source_event_id WHERE lot.target_id=$1`, target.TargetID).Scan(&amount, &eventID, &eligible, &generation, &commitment); err != nil {
		t.Fatal(err)
	}
	if amount != 5_000_000 || generation != 3 || eventID > -2_000_000_000_000 || eventID < -2_999_999_999_999 || eligible.Before(time.Unix(start, 0)) || commitment != "confirmed" {
		t.Fatalf("bootstrap amount=%d event=%d generation=%d eligible=%s commitment=%s", amount, eventID, generation, eligible, commitment)
	}
	if err := s.pool.QueryRow(ctx, `SELECT requested_slot,processed_slot FROM loyal_yield.autodeposit_reconciliation_requests WHERE target_id=$1`, target.TargetID).Scan(&requested, &processed); err != nil {
		t.Fatal(err)
	}
	if requested != 990002 || processed != 990001 {
		t.Fatalf("high water requested=%d processed=%d", requested, processed)
	}
	var projectedAmount, projectedSlot int64
	var projectedHash string
	if err := s.pool.QueryRow(ctx, `SELECT amount_raw,observed_slot,account_data_hash FROM loyal_yield.balance_sweep_wallet_balances_current WHERE target_id=$1 AND mint=$2`, target.TargetID, USDCMint).Scan(&projectedAmount, &projectedSlot, &projectedHash); err != nil || projectedAmount != o.WalletBalanceRaw || projectedSlot != o.ObservedSlot || projectedHash != o.WalletAccountDataSHA256 {
		t.Fatalf("bootstrap projection amount=%d slot=%d hash=%s err=%v", projectedAmount, projectedSlot, projectedHash, err)
	}
	r = claimControlRequest(t, s, target.TargetID, 990002, "control-next")
	o.ObservedSlot = 990003
	if err := s.ApplyControlObservation(ctx, r, "control-next", o); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*)FROM loyal_yield.balance_sweep_surplus_lots WHERE target_id=$1`, target.TargetID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate bootstrap count=%d err=%v", count, err)
	}
	// A newer wallet observation from the account stream retains authority
	// over an older control snapshot, including its floor-rebase balance.
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_wallet_balances_current SET amount_raw=4000000,observed_slot=990010,source_commitment='finalized' WHERE target_id=$1`, target.TargetID); err != nil {
		t.Fatal(err)
	}
	r = claimControlRequest(t, s, target.TargetID, 990004, "control-projection")
	o.ObservedSlot = 990005
	if err := s.ApplyControlObservation(ctx, r, "control-projection", o); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT amount_raw,observed_slot FROM loyal_yield.balance_sweep_wallet_balances_current WHERE target_id=$1 AND mint=$2`, target.TargetID, USDCMint).Scan(&projectedAmount, &projectedSlot); err != nil || projectedAmount != 4_000_000 || projectedSlot != 990010 {
		t.Fatalf("wallet projection regressed amount=%d slot=%d err=%v", projectedAmount, projectedSlot, err)
	}
}

func TestControlLeaseGenerationAndUnknownFloor(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	target := seedControlTarget(t, s, "control-fences")
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET wallet_balance_floor_raw=NULL WHERE id=$1`, target.TargetID); err != nil {
		t.Fatal(err)
	}
	o := ControlObservation{Target: target, ObservedSlot: 990010, PolicyExists: true, DelegationExists: true, PolicyValid: true, AuthorityValid: true, DelegationValid: true, TokenDelegateValid: true, WalletBalanceRaw: 9_000_000, WalletAccountDataSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	r := claimControlRequest(t, s, target.TargetID, 990009, "control-floor")
	if err := s.ApplyControlObservation(ctx, r, "another-owner", o); err == nil {
		t.Fatal("displaced lease applied observation")
	}
	if err := s.ApplyControlObservation(ctx, r, "control-floor", o); err != nil {
		t.Fatal(err)
	}
	var generation *int64
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT bootstrap_generation,(SELECT COUNT(*)FROM loyal_yield.balance_sweep_surplus_lots WHERE target_id=$1)FROM loyal_yield.balance_sweep_targets WHERE id=$1`, target.TargetID).Scan(&generation, &count); err != nil || generation != nil || count != 0 {
		t.Fatalf("unknown floor bootstrap=%v lots=%d err=%v", generation, count, err)
	}
	r = claimControlRequest(t, s, target.TargetID, 990011, "control-generation")
	o.ObservedSlot = 990012
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET setup_generation=setup_generation+1 WHERE id=$1`, target.TargetID); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyControlObservation(ctx, r, "control-generation", o); err == nil {
		t.Fatal("stale generation observation applied")
	}
}

func TestControlClosedTargetCannotResurrect(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	target := seedControlTarget(t, s, "control-close")
	r := claimControlRequest(t, s, target.TargetID, 990020, "control-close")
	o := ControlObservation{Target: target, ObservedSlot: 990020}
	if err := s.ApplyControlObservation(ctx, r, "control-close", o); err != nil {
		t.Fatal(err)
	}
	r = claimControlRequest(t, s, target.TargetID, 990021, "control-resurrect")
	o.ObservedSlot = 990021
	o.PolicyExists = true
	o.DelegationExists = true
	o.PolicyValid = true
	o.AuthorityValid = true
	o.DelegationValid = true
	o.TokenDelegateValid = true
	o.WalletAccountDataSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := s.ApplyControlObservation(ctx, r, "control-resurrect", o); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.pool.QueryRow(ctx, `SELECT chain_status FROM loyal_yield.balance_sweep_targets WHERE id=$1`, target.TargetID).Scan(&status); err != nil || status != "closed" {
		t.Fatalf("closed target status=%s err=%v", status, err)
	}
}

// Bootstrap on a vault without an Earn route would schedule work dispatch can
// never select; it waits for the route instead and then schedules the
// surplus observed at that point.
func TestControlBootstrapWaitsForEarnRoute(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	target := seedControlTarget(t, s, "control-unrouted")
	setRoute := func(active bool) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.route_policies AS policy SET active=$2 FROM loyal_yield.managed_vaults AS vault, loyal_yield.balance_sweep_targets AS target WHERE target.id=$1 AND vault.settings=target.settings AND policy.id=vault.active_policy_id`, target.TargetID, active); err != nil {
			t.Fatal(err)
		}
	}
	state := func() (lots int, bootstrap *int64) {
		t.Helper()
		if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM loyal_yield.balance_sweep_surplus_lots WHERE target_id=$1),bootstrap_generation FROM loyal_yield.balance_sweep_targets WHERE id=$1`, target.TargetID).Scan(&lots, &bootstrap); err != nil {
			t.Fatal(err)
		}
		return lots, bootstrap
	}
	o := ControlObservation{Target: target, ObservedSlot: 991001, PolicyExists: true, DelegationExists: true, PolicyValid: true, AuthorityValid: true, DelegationValid: true, TokenDelegateValid: true, WalletBalanceRaw: 9_000_000, WalletAccountDataSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	setRoute(false)
	if err := s.ApplyControlObservation(ctx, claimControlRequest(t, s, target.TargetID, 991000, "control-unrouted"), "control-unrouted", o); err != nil {
		t.Fatal(err)
	}
	if lots, bootstrap := state(); lots != 0 || bootstrap != nil {
		t.Fatalf("unrouted bootstrap lots=%d generation=%v, want no lot and bootstrap pending", lots, bootstrap)
	}
	setRoute(true)
	o.ObservedSlot = 991003
	if err := s.ApplyControlObservation(ctx, claimControlRequest(t, s, target.TargetID, 991002, "control-routed"), "control-routed", o); err != nil {
		t.Fatal(err)
	}
	if lots, bootstrap := state(); lots != 1 || bootstrap == nil || *bootstrap != 3 {
		t.Fatalf("routed bootstrap lots=%d generation=%v, want one lot at generation 3", lots, bootstrap)
	}
}
