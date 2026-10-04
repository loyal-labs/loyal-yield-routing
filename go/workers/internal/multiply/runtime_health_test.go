package multiply

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRuntimeReporterClosedOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := &Worker{}
	reports := 0
	w.SetRuntimeReporter(func(ready bool, slot uint64) {
		reports++
		if ready {
			t.Fatal("canceled runtime reported ready")
		}
	})
	_ = w.Run(ctx)
	if reports < 2 {
		t.Fatal("startup and joined exit did not both close readiness")
	}
}

func TestRuntimeConditionNeverTreatsKnownHoldsAsSuccess(t *testing.T) {
	for _, condition := range []string{"manual_recovery_required", "expired_signature_unresolved_ownership_retained", "legacy_expired_attempt_missing_prestate_ownership_retained", "awaiting_coherent_confirmed_observation", "earn_max_policy_set_not_ready", "withdrawal_exact_payout_shortfall"} {
		if runtimeConditionKnown(condition) {
			t.Fatalf("hold %s reported ready", condition)
		}
	}
}

// Registered database state + local JSON-RPC frontier exercise health census
// plumbing only. This is not evidence of financial execution on a real chain.
func TestRuntimeCensusCannotHidePreparedWorkOrMissingSnapshots(t *testing.T) {
	store := integrationStore(t)
	state, _ := runtimeFixtureRoute(t, store)
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, in *http.Request) {
		var req struct {
			ID     int64             `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(in.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch req.Method {
		case "getLatestBlockhash":
			result = map[string]any{"context": map[string]any{"slot": 800}, "value": map[string]any{"blockhash": "11111111111111111111111111111111", "lastValidBlockHeight": 900}}
		case "getBlockHeight":
			var commitment map[string]string
			if len(req.Params) != 1 || json.Unmarshal(req.Params[0], &commitment) != nil || commitment["commitment"] != "finalized" {
				t.Error("health height must be finalized")
			}
			result = 700
		default:
			t.Errorf("unexpected health method %s", req.Method)
		}
		_ = json.NewEncoder(out).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer server.Close()
	w := &Worker{store: store, routeKey: &state.RouteKey, executor: &Executor{RPC: NewLiveRPCSurface(server.URL)}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	idle := TickResult{Condition: "no_route_available"}
	if _, err := w.runtimeRecoveryHealth(ctx, idle); err == nil {
		t.Fatal("missing position evidence became healthy")
	}
	if _, err := store.RecordPositionSnapshot(ctx, &PositionSnapshotInput{RouteKey: state.RouteKey, Generation: state.Generation, ObservedSlot: 500, ObservedAt: time.Now().UTC(), ClaimRaw: "0", CollateralRaw: "0", DebtRaw: "0", EquityUSD: "0", CollateralValueUSD: "0", DebtValueUSD: "0", ValuationSource: "confirmed_kamino_reserve_curve_500ms"}); err != nil {
		t.Fatal(err)
	}
	if slot, err := w.runtimeRecoveryHealth(ctx, idle); err != nil || slot != 800 {
		t.Fatalf("verified idle census %d %v", slot, err)
	}
	lease, err := store.LeaseRoute(ctx, state.RouteKey, "health-fixture", time.Now().Add(time.Minute))
	if err != nil || lease == nil {
		t.Fatalf("lease %v %v", lease, err)
	}
	// Synthetic source records exercise only health classification, not receipt
	// proof: a user-owned claimable wait needs the latest exact payout coverage.
	completed := time.Now().UTC().Add(-10 * time.Minute)
	state.Generation++
	state.Goal = GoalWithdraw
	state.Withdrawal = &Withdrawal{RequestID: "health-request", DestinationAccount: state.Position.Claim.Account, AmountRaw: 1, Status: WithdrawalClaimable, RequestedAt: completed.Add(-time.Minute), ReadyBy: completed.Add(9 * time.Minute), UnwindCompletedAt: &completed}
	if ok, err := store.SaveRouteState(ctx, lease, state); err != nil || !ok {
		t.Fatalf("claim-wait fixture %v %v", ok, err)
	}
	if _, err := w.runtimeRecoveryHealth(ctx, idle); err == nil {
		t.Fatal("fresh snapshot shortfall reported healthy claim wait")
	}
	if _, err := store.RecordPositionSnapshot(ctx, &PositionSnapshotInput{RouteKey: state.RouteKey, Generation: state.Generation, ObservedSlot: 501, ObservedAt: completed, ClaimRaw: "1", CollateralRaw: "0", DebtRaw: "0", EquityUSD: "1", CollateralValueUSD: "0", DebtValueUSD: "0", ValuationSource: "confirmed_kamino_reserve_curve_500ms"}); err != nil {
		t.Fatal(err)
	}
	if slot, err := w.runtimeRecoveryHealth(ctx, idle); err != nil || slot != 800 {
		t.Fatalf("source-completed wallet wait %d %v", slot, err)
	}
	state.Generation++
	state.Goal = GoalIdle
	state.Withdrawal = nil
	if ok, err := store.SaveRouteState(ctx, lease, state); err != nil || !ok {
		t.Fatalf("reset source health fixture %v %v", ok, err)
	}
	operation := integrationOperation(state.RouteKey, state.Cycle, lease.Version+1)
	state.Generation++
	state.CurrentOperationID = &operation.OperationID
	if ok, err := store.PrepareOperation(ctx, lease, state, operation); err != nil || !ok {
		t.Fatalf("prepare %v %v", ok, err)
	}
	// The row is held by a real live source lease: no_route_available must still
	// close health because the complete census can see the pending ownership.
	if _, err := w.runtimeRecoveryHealth(ctx, idle); err == nil {
		t.Fatal("lease-held prepared work disappeared from health")
	}
	if ok, err := store.ReleaseLease(ctx, lease); err != nil || !ok {
		t.Fatalf("release %v %v", ok, err)
	}
}
