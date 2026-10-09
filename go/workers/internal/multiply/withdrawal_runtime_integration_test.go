package multiply

import (
	"context"
	"testing"
	"time"
)

func TestWorkerWithdrawalShortfallPersistsAcrossRestartWithoutChangingPayout(t *testing.T) {
	store := integrationStore(t)
	state, topology := runtimeFixtureRoute(t, store)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	lease, err := store.LeaseRoute(ctx, state.RouteKey, "request-owner", time.Now().Add(time.Minute))
	if err != nil || lease == nil {
		t.Fatalf("request lease %v %v", lease, err)
	}
	destination := fixtureKey(213)
	requested := time.Now().UTC().Add(-time.Minute)
	state.Generation++
	state.Goal = GoalWithdraw
	state.Withdrawal = &Withdrawal{RequestID: "requested-exact-payout", DestinationAccount: destination.String(), AmountRaw: 10_000_000, Status: WithdrawalRequested, RequestedAt: requested, ReadyBy: requested.Add(10 * time.Minute)}
	if ok, err := store.SaveRouteState(ctx, lease, state); err != nil || !ok {
		t.Fatalf("request write %v %v", ok, err)
	}
	if ok, err := store.ReleaseLease(ctx, lease); err != nil || !ok {
		t.Fatalf("request release %v %v", ok, err)
	}
	reader := emptyBankReader(topology)
	reader.accounts[topology.ClaimCustody.String()] = observedToken(topology.ClaimCustody, USDCMint, topology.Vault, 9_990_000)
	reader.accounts[destination.String()] = observedToken(destination, USDCMint, fixtureKey(214), 0)
	newWorker := func(owner string) *Worker {
		t.Helper()
		executor, fake, _ := testExecutor(t)
		worker, err := NewWorker(WorkerDeps{Store: store, Observer: reader, Executor: executor, Quotes: fakeQuoteClient{topology}, WorkerID: owner, RouteKey: &state.RouteKey, Chain: fake, Facts: testFacts()})
		if err != nil {
			t.Fatal(err)
		}
		return worker
	}
	for _, owner := range []string{"first-worker", "restarted-worker"} {
		result, err := newWorker(owner).Tick(ctx)
		if err != nil || result.Condition != "withdrawal_liquidity_shortfall" {
			t.Fatalf("%s shortfall %v %v", owner, result, err)
		}
		saved, err := store.LoadRouteState(ctx, state.RouteKey)
		if err != nil {
			t.Fatal(err)
		}
		if saved.State.Goal != GoalWithdraw || saved.State.Withdrawal.AmountRaw != 10_000_000 || saved.State.Withdrawal.Status != WithdrawalRequested || saved.State.CurrentOperationID != nil {
			t.Fatal("shortfall changed intent or declared claimable")
		}
		var claim, collateral, debt string
		if err := store.Pool().QueryRow(ctx, `SELECT claim_raw::text,collateral_raw::text,debt_raw::text FROM loyal_yield.multiply_position_snapshots WHERE route_key=$1 ORDER BY observed_slot DESC,id DESC LIMIT 1`, state.RouteKey).Scan(&claim, &collateral, &debt); err != nil {
			t.Fatal(err)
		}
		if claim != "9990000" || collateral != "0" || debt != "0" {
			t.Fatal("shortfall lacks durable confirmed financial evidence")
		}
	}
	// A missing current destination observation never reuses an old balance
	// to advance the withdrawal. Intent remains intact for later fresh evidence.
	delete(reader.accounts, destination.String())
	if _, err := newWorker("missing-observation-worker").Tick(ctx); err == nil {
		t.Fatal("missing latest destination declared claimable")
	}
	reader.accounts[destination.String()] = observedToken(destination, USDCMint, fixtureKey(214), 0)
	reader.accounts[topology.ClaimCustody.String()] = observedToken(topology.ClaimCustody, USDCMint, topology.Vault, 10_000_000)
	reader.slot++
	result, err := newWorker("funded-worker").Tick(ctx)
	if err != nil || result.Condition != "route_complete" {
		t.Fatalf("exact payout completion %v %v", result, err)
	}
	saved, err := store.LoadRouteState(ctx, state.RouteKey)
	if err != nil {
		t.Fatal(err)
	}
	if saved.State.Withdrawal.Status != WithdrawalClaimable || saved.State.Withdrawal.AmountRaw != 10_000_000 || saved.State.Withdrawal.UnwindCompletedAt == nil {
		t.Fatal("exact confirmed liquidity failed claimable transition")
	}
}
