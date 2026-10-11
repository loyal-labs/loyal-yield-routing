package multiply

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// seedIntent records one memo intent as the observer does.
func seedIntent(t *testing.T, store *Store, route *RouteState, signature, kind, requestID string, amount *string, destination *string, slot int64) {
	t.Helper()
	if _, err := store.Pool().Exec(context.Background(), `
        INSERT INTO loyal_yield.earn_max_intents (signature, instruction_index, settings, vault, kind, request_id, amount_raw, destination, slot, observed_at)
        VALUES ($1, 256, $2, $3, $4, $5, $6::numeric, $7, $8, now() - interval '1 minute')`,
		signature, route.Settings, route.Vault, kind, requestID, amount, destination, slot); err != nil {
		t.Fatal(err)
	}
}

func intentDecision(t *testing.T, store *Store, signature string) (status string, reason *string) {
	t.Helper()
	if err := store.Pool().QueryRow(context.Background(), `SELECT status, refusal_reason FROM loyal_yield.earn_max_intents
        WHERE signature=$1 AND instruction_index=256 AND (status='pending') = (decided_at IS NULL)`, signature).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	return status, reason
}

func intentWorker(t *testing.T, store *Store, route *RouteState, topology *EarnMaxTopology, reader *bankObservationReader) *Worker {
	t.Helper()
	executor, fake, _ := testExecutor(t)
	worker, err := NewWorker(WorkerDeps{Store: store, Observer: reader, Executor: executor, Quotes: fakeQuoteClient{topology}, WorkerID: "intent-worker", RouteKey: &route.RouteKey, Chain: fake, Facts: testFacts()})
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func text(value string) *string { return &value }

// The leaseholder applies a withdraw memo before planning: the route's goal
// becomes withdraw with the memo's exact request, the request_withdrawal
// operation is recorded, and the next tick makes the payout claimable.
func TestWorkerAppliesWithdrawIntentThenMakesItClaimable(t *testing.T) {
	store := integrationStore(t)
	route, topology := runtimeFixtureRoute(t, store)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	destination := fixtureKey(221)
	signature := fmt.Sprintf("withdraw-%d", time.Now().UnixNano())
	seedIntent(t, store, route, signature, "withdraw", "request-applied", text("3000000"), text(destination.String()), 600)
	reader := emptyBankReader(topology)
	worker := intentWorker(t, store, route, topology, reader)
	if result, err := worker.Tick(ctx); err != nil || result.Condition != "earn_max_intents_applied" {
		t.Fatalf("apply %v %v", result, err)
	}
	saved, err := store.LoadRouteState(ctx, route.RouteKey)
	if err != nil {
		t.Fatal(err)
	}
	withdrawal := saved.State.Withdrawal
	if saved.State.Goal != GoalWithdraw || withdrawal == nil || withdrawal.RequestID != "request-applied" || withdrawal.AmountRaw != 3_000_000 ||
		withdrawal.DestinationAccount != destination.String() || withdrawal.Status != WithdrawalRequested || time.Since(withdrawal.RequestedAt) < time.Minute {
		t.Fatalf("route after intent %+v", saved.State)
	}
	var action, status string
	if err := store.Pool().QueryRow(ctx, `SELECT action, status FROM loyal_yield.multiply_operations WHERE route_key=$1 AND transaction_signature=$2`, route.RouteKey, signature).Scan(&action, &status); err != nil || action != "request_withdrawal" || status != "reconciled" {
		t.Fatalf("operation %s %s %v", action, status, err)
	}
	if status, _ := intentDecision(t, store, signature); status != "applied" {
		t.Fatalf("intent %s", status)
	}
	reader.accounts[destination.String()] = observedToken(destination, USDCMint, fixtureKey(222), 0)
	if result, err := worker.Tick(ctx); err != nil || result.Condition != "route_complete" {
		t.Fatalf("claimable %v %v", result, err)
	}
	saved, err = store.LoadRouteState(ctx, route.RouteKey)
	if err != nil || saved.State.Withdrawal.Status != WithdrawalClaimable {
		t.Fatalf("withdrawal not claimable %v", err)
	}
}

// A second withdrawal while one is pending, a reused request id and a cancel
// of an unknown request are refused with their reasons and never retried; a
// cancel of the pending request is applied.
func TestWorkerRefusesIntentsTheRouteCannotTake(t *testing.T) {
	store := integrationStore(t)
	route, topology := runtimeFixtureRoute(t, store)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	destination := fixtureKey(223).String()
	prefix := fmt.Sprintf("refuse-%d-", time.Now().UnixNano())
	seedIntent(t, store, route, prefix+"first", "withdraw", "request-first", text("1000000"), &destination, 600)
	seedIntent(t, store, route, prefix+"second", "withdraw", "request-second", text("1000000"), &destination, 601)
	seedIntent(t, store, route, prefix+"reused", "withdraw", "request-first", text("2000000"), &destination, 602)
	seedIntent(t, store, route, prefix+"unknown", "cancel", "request-unknown", nil, nil, 603)
	seedIntent(t, store, route, prefix+"cancel", "cancel", "request-first", nil, nil, 604)
	worker := intentWorker(t, store, route, topology, emptyBankReader(topology))
	if result, err := worker.Tick(ctx); err != nil || result.Condition != "earn_max_intents_applied" {
		t.Fatalf("decide %v %v", result, err)
	}
	for name, want := range map[string]string{"first": "", "second": "withdrawal_already_pending", "reused": "request_id_reused", "unknown": "unknown_request", "cancel": ""} {
		status, reason := intentDecision(t, store, prefix+name)
		if want == "" && (status != "applied" || reason != nil) || want != "" && (status != "refused" || reason == nil || *reason != want) {
			t.Errorf("%s: %s %v, want %q", name, status, reason, want)
		}
	}
	saved, err := store.LoadRouteState(ctx, route.RouteKey)
	if err != nil || saved.State.Goal != GoalDeploy || saved.State.Withdrawal != nil {
		t.Fatalf("cancel not applied %v", err)
	}
	var cancels int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM loyal_yield.multiply_operations WHERE route_key=$1 AND action='cancel_withdrawal'`, route.RouteKey).Scan(&cancels); err != nil || cancels != 1 {
		t.Fatalf("cancel operations %d %v", cancels, err)
	}
	if pending, err := store.pendingIntents(ctx, route.Settings, route.Vault); err != nil || len(pending) != 0 {
		t.Fatalf("decided intents left pending %v %v", pending, err)
	}
}

// A "max" withdrawal is the equity the worker observes when it applies it.
func TestWorkerResolvesMaxWithdrawalFromObservedEquity(t *testing.T) {
	store := integrationStore(t)
	route, topology := runtimeFixtureRoute(t, store)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	reader := emptyBankReader(topology)
	reader.accounts[topology.ClaimCustody.String()] = observedToken(topology.ClaimCustody, USDCMint, topology.Vault, 7_250_000)
	seedIntent(t, store, route, fmt.Sprintf("max-%d", time.Now().UnixNano()), "withdraw", "request-max", nil, text(fixtureKey(224).String()), 600)
	if result, err := intentWorker(t, store, route, topology, reader).Tick(ctx); err != nil || result.Condition != "earn_max_intents_applied" {
		t.Fatalf("apply %v %v", result, err)
	}
	saved, err := store.LoadRouteState(ctx, route.RouteKey)
	if err != nil || saved.State.Withdrawal == nil || saved.State.Withdrawal.AmountRaw != 7_250_000 {
		t.Fatalf("max withdrawal %+v %v", saved.State.Withdrawal, err)
	}
}

// A claimable withdrawal is not leased for planning, so a pending cancel is
// what hands the route to a worker, which applies it.
func TestPendingCancelLeasesAClaimableRouteAndApplies(t *testing.T) {
	store := integrationStore(t)
	route, topology := runtimeFixtureRoute(t, store)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	lease, err := store.LeaseRoute(ctx, route.RouteKey, "request-owner", time.Now().Add(time.Minute))
	if err != nil || lease == nil {
		t.Fatalf("lease %v %v", lease, err)
	}
	requested := time.Now().UTC().Add(-time.Minute)
	route.Generation++
	route.Goal = GoalWithdraw
	route.Withdrawal = &Withdrawal{RequestID: "request-claimable", DestinationAccount: fixtureKey(225).String(), AmountRaw: 1_000_000, Status: WithdrawalClaimable, RequestedAt: requested, ReadyBy: requested.Add(10 * time.Minute)}
	if ok, err := store.SaveRouteState(ctx, lease, route); err != nil || !ok {
		t.Fatalf("claimable write %v %v", ok, err)
	}
	if ok, err := store.ReleaseLease(ctx, lease); err != nil || !ok {
		t.Fatalf("release %v %v", ok, err)
	}
	leasedOurs := func() bool {
		for {
			next, err := store.LeaseNextRoute(ctx, "eligibility", time.Now().Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if next == nil || next.RouteKey == route.RouteKey {
				if next != nil {
					if ok, err := store.ReleaseLease(ctx, next); err != nil || !ok {
						t.Fatalf("release %v %v", ok, err)
					}
				}
				return next != nil
			}
		}
	}
	if leasedOurs() {
		t.Fatal("claimable route leased without an intent")
	}
	signature := fmt.Sprintf("cancel-%d", time.Now().UnixNano())
	seedIntent(t, store, route, signature, "cancel", "request-claimable", nil, nil, 600)
	if !leasedOurs() {
		t.Fatal("pending cancel did not make the route leasable")
	}
	reader := emptyBankReader(topology)
	reader.accounts[fixtureKey(225).String()] = observedToken(fixtureKey(225), USDCMint, fixtureKey(226), 0)
	if result, err := intentWorker(t, store, route, topology, reader).Tick(ctx); err != nil || result.Condition != "earn_max_intents_applied" {
		t.Fatalf("cancel %v %v", result, err)
	}
	saved, err := store.LoadRouteState(ctx, route.RouteKey)
	if err != nil || saved.State.Goal != GoalDeploy || saved.State.Withdrawal != nil {
		t.Fatalf("cancel not applied %v", err)
	}
	if status, _ := intentDecision(t, store, signature); status != "applied" {
		t.Fatalf("intent %s", status)
	}
}
