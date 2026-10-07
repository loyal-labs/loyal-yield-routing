package multiply

import (
	"context"
	"testing"
	"time"
)

func TestWorkerWithdrawalIsClaimableAfterFeeLossWithoutRewritingTheRequest(t *testing.T) {
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
		executor, _, _ := testExecutor(t)
		worker, err := NewWorker(WorkerDeps{Store: store, Observer: reader, Executor: executor, Quotes: fakeQuoteClient{topology}, WorkerID: owner, RouteKey: &state.RouteKey, Chain: surfaceChain{executor.RPC}, Facts: testFacts()})
		if err != nil {
			t.Fatal(err)
		}
		return worker
	}
	// A missing current destination observation never reuses an old balance
	// to advance the withdrawal.
	delete(reader.accounts, destination.String())
	if _, err := newWorker("missing-observation-worker").Tick(ctx); err == nil {
		t.Fatal("missing latest destination declared claimable")
	}
	reader.accounts[destination.String()] = observedToken(destination, USDCMint, fixtureKey(214), 0)
	reader.slot++
	// Fees left 9.99 against a saved 10: the unwind is complete, so the
	// withdrawal is claimable and the payout is built from the custody.
	result, err := newWorker("unwound-worker").Tick(ctx)
	if err != nil || result.Condition != "route_complete" {
		t.Fatalf("fee-loss completion %v %v", result, err)
	}
	saved, err := store.LoadRouteState(ctx, state.RouteKey)
	if err != nil {
		t.Fatal(err)
	}
	if saved.State.Withdrawal.Status != WithdrawalClaimable || saved.State.Withdrawal.AmountRaw != 10_000_000 || saved.State.Withdrawal.UnwindCompletedAt == nil {
		t.Fatal("unwound withdrawal was not claimable or its saved request changed")
	}
}
