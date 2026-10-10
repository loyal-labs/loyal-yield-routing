package backyard

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestWithdrawalHealthPreservesAttentionUntilCurrentRecovery(t *testing.T) {
	o := Observation{ObservedAt: time.Unix(1000, 0).UTC(), Snapshot: livePartialSnapshot()}
	h, ok := assessWithdrawalHealth(o, Decision{Action: Hold, Reason: "withdrawal_full_exit_unproven"}, nil)
	if !ok || h.Status != "operator_attention" {
		t.Fatalf("known blocker: %+v", h)
	}
	h = mergeWithdrawalHealth(nil, h)
	// Serialize/reload exactly like a worker restart; no in-memory onset timer.
	raw, _ := json.Marshal(h)
	var previous WithdrawalHealth
	if err := json.Unmarshal(raw, &previous); err != nil {
		t.Fatal(err)
	}
	o.ObservedAt = o.ObservedAt.Add(time.Minute)
	o.Snapshot.Slot++
	for _, d := range []Decision{{Action: ReportNAV}, {Action: RecoverTransaction}, {Action: Hold}} {
		unknown, _ := assessWithdrawalHealth(o, d, nil)
		kept := mergeWithdrawalHealth(&previous, unknown)
		if kept.Status != "operator_attention" || !kept.ObservedAt.Equal(previous.ObservedAt) || !kept.BlockedSince.Equal(*previous.BlockedSince) {
			t.Fatalf("non-funding evidence erased attention: %+v", kept)
		}
	}
	unknown, _ := assessWithdrawalHealth(o, Decision{}, errors.New("RPC failed"))
	if kept := mergeWithdrawalHealth(&previous, unknown); kept.Status != "operator_attention" {
		t.Fatal("read error recovered")
	}
	o.Snapshot.VoltrIdleRaw = o.Snapshot.WithdrawalDemandRaw
	covered, _ := assessWithdrawalHealth(o, Decision{}, nil)
	covered = mergeWithdrawalHealth(&previous, covered)
	if covered.Status != "waiting" || covered.Reason != "withdrawal_covered" || covered.BlockedSince != nil {
		t.Fatalf("covered: %+v", covered)
	}
	o.Snapshot.WithdrawalDemandRaw = 0
	none, _ := assessWithdrawalHealth(o, Decision{}, nil)
	if none.Status != "none" {
		t.Fatalf("no demand: %+v", none)
	}
	o.Snapshot.Fresh = false
	if _, ok := assessWithdrawalHealth(o, Decision{}, nil); ok {
		t.Fatal("unknown zero demand became recovery")
	}
}

func TestWithdrawalHealthCurrentHoldBeforeOperationExists(t *testing.T) {
	o := Observation{ObservedAt: time.Now().UTC(), Snapshot: livePartialSnapshot()}
	var stored WithdrawalHealth
	w := Worker{runtime: tickRuntime{withdrawalHealth: func(_ context.Context, h WithdrawalHealth) error { stored = h; return nil }}}
	// Prepare may return no observation or operation ID. The tick retains its
	// earlier coherent snapshot and projects the current admission refusal.
	w.publishWithdrawalHealth(context.Background(), o, Decision{Action: DeleverRouteStep}, budgetHold("debt_clear_confirmation_required"))
	if stored.Status != "operator_attention" || !stored.ObservedAt.Equal(o.ObservedAt) || stored.ObservedSlot != o.Snapshot.Slot {
		t.Fatalf("pre-operation hold lost: %+v", stored)
	}
	stored = WithdrawalHealth{}
	o.Snapshot.WithdrawalDemandRaw = 0
	w.publishWithdrawalHealth(context.Background(), o, Decision{Action: DeleverRouteStep}, budgetHold("debt_clear_confirmation_required"))
	if stored.Status != "none" {
		t.Fatal("unrelated optimizer hold became withdrawal alert")
	}
}

// Opt-in existing disposable-Postgres harness; never accepts a remote database.
func TestWithdrawalHealthFencedPersistenceDoesNotChangeAuthority(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 15*time.Second)
	defer cancel()
	defer db.Close()
	resetManualRecoveryProductionRoute(t, ctx, db, "withdrawal-health-worker")
	o := Observation{ObservedAt: time.Now().UTC(), Snapshot: livePartialSnapshot()}
	h, _ := assessWithdrawalHealth(o, Decision{Action: Hold, Reason: "withdrawal_full_exit_unproven"}, nil)
	if err := db.RecordWithdrawalHealth(ctx, h); err != nil {
		t.Fatal(err)
	}
	read := func() (WithdrawalHealth, int64, int64) {
		t.Helper()
		var raw []byte
		var version, generation int64
		if err := db.pool.QueryRow(ctx, `SELECT state->'withdrawalHealth',state_version,(state->>'generation')::bigint FROM loyal_yield.multiply_route_states WHERE route_key=$1`, productionRouteKey).Scan(&raw, &version, &generation); err != nil {
			t.Fatal(err)
		}
		var stored WithdrawalHealth
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatal(err)
		}
		return stored, version, generation
	}
	first, version, generation := read()
	if first.Status != "operator_attention" || first.BlockedSince == nil || version != 1 || generation != 1 {
		t.Fatalf("state/authority changed: %+v %d %d", first, version, generation)
	}
	h.ObservedAt = h.ObservedAt.Add(time.Minute)
	h.ObservedSlot++
	if err := db.RecordWithdrawalHealth(ctx, h); err != nil {
		t.Fatal(err)
	}
	second, _, _ := read()
	if !second.BlockedSince.Equal(*first.BlockedSince) {
		t.Fatal("durable onset reset")
	}
	// A different DB lease owner cannot rewrite recovery or publish healthy state.
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET fencing_token=fencing_token+1 WHERE route_key=$1`, productionRouteKey); err != nil {
		t.Fatal(err)
	}
	h.Status, h.Reason = "none", "no_withdrawal_demand"
	if err := db.RecordWithdrawalHealth(ctx, h); err == nil {
		t.Fatal("stale fence wrote recovery")
	}
	final, version, generation := read()
	if final.Status != "operator_attention" || version != 1 || generation != 1 {
		t.Fatal("failed write changed health/authority")
	}
}

func TestTickPersistsWithdrawalPrepareHoldBeforeRecordingOperation(t *testing.T) {
	o := Observation{ObservedAt: time.Now().UTC(), Snapshot: livePartialSnapshot()}
	var stored WithdrawalHealth
	recorded := false
	w := Worker{routeKey: productionRouteKey, manifest: readyWorkerManifest(t), runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe:         func(context.Context) (Observation, error) { return o, nil },
		prepareKamino: func(context.Context, RouteManifest, Decision) (Observation, KaminoExecutionEvidence, error) {
			return Observation{}, KaminoExecutionEvidence{}, budgetHold("squads_spending_limit_exceeded")
		},
		recordDecision: func(context.Context, string, Observation, Decision, string) (DecisionRecord, error) {
			recorded = true
			return DecisionRecord{}, errors.New("must not create operation")
		},
		withdrawalHealth: func(_ context.Context, h WithdrawalHealth) error { stored = h; return nil },
	}}
	err := w.Tick(context.Background())
	var hold *BudgetHold
	if !errors.As(err, &hold) || hold.Reason != "squads_spending_limit_exceeded" || recorded || stored.Status != "operator_attention" {
		t.Fatalf("pre-operation refusal not projected: err=%v recorded=%v health=%+v", err, recorded, stored)
	}
}

func TestWithdrawalHealthDoesNotObserveDuringRecoveryOrReadFailure(t *testing.T) {
	reads, writes := 0, 0
	w := Worker{runtime: tickRuntime{
		observeWithdrawalHealth: func(context.Context) (Observation, error) {
			reads++
			return Observation{ObservedAt: time.Now().UTC(), Snapshot: livePartialSnapshot()}, nil
		},
		withdrawalHealth: func(ctx context.Context, _ WithdrawalHealth) error {
			writes++
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 2*time.Second {
				t.Fatal("display write deadline is not bounded")
			}
			return nil
		},
	}}
	w.publishWithdrawalHealth(context.Background(), Observation{}, Decision{Action: RecoverTransaction}, nil)
	w.publishWithdrawalHealth(context.Background(), Observation{}, Decision{}, errors.New("observation failed"))
	if reads != 0 || writes != 0 {
		t.Fatal("display added reads/writes during recovery or read failure")
	}
	w.publishWithdrawalHealth(context.Background(), Observation{}, Decision{Action: HoldManualRecovery}, nil)
	if reads != 1 || writes != 1 {
		t.Fatal("paused manual stop lost display observation")
	}
}
