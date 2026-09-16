package backyardrwa

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

func bridgeAdmissionFixture(t *testing.T, action Action, amount, idle, strategy, squads int64) (Observation, Decision, BridgeExecutionEvidence) {
	t.Helper()
	s := Snapshot{ObservationID: "bridge-admission", Slot: 42, Fresh: true, RouteKind: RouteKind,
		RouteLane: ethenaUSDePYUSD.Lane, StrategyKey: ethenaUSDePYUSD.Lane,
		VoltrIdleRaw: idle, VoltrStrategyIdleRaw: strategy, SquadsIdleRaw: squads}
	d := Decision{Action: action, AmountRaw: amount, StrategyKey: s.RouteLane, Reason: "bridge-admission-test", IdempotencyKey: "bridge-admission-test"}
	r := bridgeTestRequest(action, uint64(amount))
	r.Report.ObservedSlot, r.Report.Sequence = 42, 42
	effects, afterStrategy, afterSquads, err := bridgeExpectedEffects(d, uint64(idle), uint64(strategy), uint64(squads))
	if err != nil {
		t.Fatal(err)
	}
	r.Report.NAVAfterRaw = afterStrategy + afterSquads
	effects.Kind = "bridge"
	if action != StageSquadsToVoltr {
		effects.ReturnData = expectedAdaptorReturnData(r.Report.NAVAfterRaw)
	}
	return tickObservation(s), d, BridgeExecutionEvidence{r, effects}
}

func TestBridgeAdmissionMeasuresCompleteCashReturnWithoutSigner(t *testing.T) {
	o, d, evidence := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 100_000, 200_000, 0, 0)
	plan, err := observePhase3BridgeAdmission(context.Background(), budgetBuildRPC(t, 5_000, 42), o, d, evidence)
	if err != nil {
		t.Fatal(err)
	}
	var actions []Action
	var total, principal int64
	for _, exit := range plan.Exit {
		actions = append(actions, exit.Action)
		total += exit.Cost.TotalMicros
		principal += exit.Cost.PrincipalMicros
		if exit.Cost.TotalMicros > Phase3TransactionCapMicros || exit.Cost.NetworkFeeMicros <= 0 {
			t.Fatal("exit dropped a fee or exceeded transaction cap")
		}
	}
	if !reflect.DeepEqual(actions, []Action{ReportNAV, StageSquadsToVoltr, ReportNAV, VoltrRestoreIdle, ReportNAV}) ||
		total != plan.ExitAfterMicros || principal != 2*plan.CurrentCost.PrincipalMicros || plan.Input == nil {
		t.Fatalf("incomplete measured return graph: %+v", plan)
	}
	// No signer/database is required to measure; accepted measurement alone
	// still cannot sign, send or initialize the missing durable goal budget.
	err = (&Database{}).admitPhase3Bridge(context.Background(), budgetBuildRPC(t, 5_000, 42), "unconfigured", o, d, evidence)
	assertBudgetHold(t, err, "bridge_admission_database_unavailable")
}

func TestBridgeAdmissionRejectsUnpricedExposureAndFullSweepCap(t *testing.T) {
	t.Run("position needs complete exit", func(t *testing.T) {
		o, d, evidence := bridgeAdmissionFixture(t, ReportNAV, 0, 0, 0, 0)
		o.Snapshot.PositionDebtRaw = 1
		_, err := observePhase3BridgeAdmission(context.Background(), nil, o, d, evidence)
		assertBudgetHold(t, err, "complete_position_exit_admission_unavailable")
	})
	t.Run("small stage cannot hide large full restore", func(t *testing.T) {
		o, d, evidence := bridgeAdmissionFixture(t, StageSquadsToVoltr, 100_000, 0, 950_000, 100_000)
		_, err := observePhase3BridgeAdmission(context.Background(), budgetBuildRPC(t, 5_000, 42), o, d, evidence)
		assertBudgetHold(t, err, "bridge_exit_or_transaction_cap_exceeded")
	})
	t.Run("partial restore", func(t *testing.T) {
		o, d, evidence := bridgeAdmissionFixture(t, VoltrRestoreIdle, 1, 0, 100_000, 0)
		_, err := observePhase3BridgeAdmission(context.Background(), nil, o, d, evidence)
		assertBudgetHold(t, err, "bridge_admission_requires_full_custody_exit")
	})
	t.Run("custody effect mismatch", func(t *testing.T) {
		o, d, evidence := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 1, 2, 0, 0)
		evidence.ExpectedEffects.Accounts[0].BeforeRaw++
		_, err := observePhase3BridgeAdmission(context.Background(), nil, o, d, evidence)
		assertBudgetHold(t, err, "bridge_admission_intent_mismatch")
	})
	t.Run("stale snapshot", func(t *testing.T) {
		o, d, evidence := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 1, 2, 0, 0)
		_, err := observePhase3BridgeAdmission(context.Background(), budgetBuildRPC(t, 5_000, 75), o, d, evidence)
		assertBudgetHold(t, err, "stale_bridge_admission_snapshot")
	})
	t.Run("existing bridge custody cannot be new allocation", func(t *testing.T) {
		o, d, evidence := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 1, 2, 1, 0)
		_, err := observePhase3BridgeAdmission(context.Background(), nil, o, d, evidence)
		assertBudgetHold(t, err, "bridge_allocation_requires_empty_strategy_custody")
	})
}

func TestBridgeAdmissionReturnGraphConsumesReservedBudget(t *testing.T) {
	ctx := context.Background()
	o, d, evidence := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 100_000, 200_000, 0, 0)
	steps, err := phase3BridgeTemplates(o.Snapshot, d, evidence)
	if err != nil {
		t.Fatal(err)
	}
	budget := emptyTestBudget()
	var spent int64
	for i, step := range steps {
		d.Action, d.AmountRaw = step.Request.Action, int64(step.Request.AmountRaw)
		plan, err := observePhase3BridgeAdmission(ctx, budgetBuildRPC(t, 5_000, 42), o, d, step)
		if err != nil {
			t.Fatal(err)
		}
		intent, err := Phase3IntentDigest(step.Request, plan.Input.Effects)
		if err != nil {
			t.Fatal(err)
		}
		r := BudgetReservation{OperationID: "current", Family: "Ethena", IntentSHA256: intent,
			UpperMicros: plan.CurrentCost.TotalMicros, ExitAfterMicros: plan.ExitAfterMicros, Recovery: i != 0}
		if err = budget.Admit(r); err != nil {
			t.Fatal(err)
		}
		// This proves accounting, not on-chain settlement. The DB test covers
		// durable admission; real finalized execution remains separate proof.
		if err = budget.Settle(r.OperationID, intent, r.UpperMicros); err != nil {
			t.Fatal(err)
		}
		spent += r.UpperMicros
		for _, effect := range step.ExpectedEffects.Accounts {
			switch effect.Address {
			case bridgeIdleATA:
				o.Snapshot.VoltrIdleRaw = int64(effect.AfterRaw)
			case bridgeStrategyATA:
				o.Snapshot.VoltrStrategyIdleRaw = int64(effect.AfterRaw)
			case bridgeSquadsATA:
				o.Snapshot.SquadsIdleRaw = int64(effect.AfterRaw)
			}
		}
	}
	if o.Snapshot.VoltrIdleRaw != 200_000 || o.Snapshot.SquadsIdleRaw != 0 || o.Snapshot.VoltrStrategyIdleRaw != 0 ||
		budget.Families["Ethena"].SpentMicros != spent || budget.Families["Ethena"].ExitMicros != 0 || len(budget.Reservations) != 0 {
		t.Fatal("return path lost custody, spend or reserve accounting")
	}
}

// Called only after the parent test has verified the disposable Unix-socket
// database and prepared its minimal schema. No production database is eligible.
func testProductionBridgeAdmission(t *testing.T, url string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key := fmt.Sprintf("phase3-bridge-producer-%d", time.Now().UnixNano())
	id := key + "-operation"
	o, d, evidence := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 100_000, 200_000, 0, 0)
	encoded, err := json.Marshal(map[string]any{"decision": newDecisionEvidence(o, d, sha256Bytes([]byte("manifest")), sha256Bytes([]byte("policies")))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,'{"generation":1}')`, key); err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects) VALUES($1,$2,'decided',$3,$4,$5::jsonb)`, id, key, d.Action, d.StrategyKey, string(encoded)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.AcquireRouteLease(ctx, key, "bridge-producer", time.Minute); err != nil {
		t.Fatal(err)
	}
	admit := func() error { return db.admitPhase3Bridge(ctx, budgetBuildRPC(t, 5_000, 42), id, o, d, evidence) }
	assertBudgetHold(t, admit(), "missing_or_mismatched_goal_budget")
	var unexpected bool
	if err = db.pool.QueryRow(ctx, `SELECT state ? 'phase3' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&unexpected); err != nil || unexpected {
		t.Fatal("producer silently initialized a missing budget", err)
	}
	// Explicit fixture initialization only; the production producer never does
	// this. Starting budget is zero, not a caller-preseeded numeric reservation.
	budget := emptyTestBudget()
	budgetBytes, err := json.Marshal(budget)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{phase3}',$2::jsonb) WHERE route_key=$1`, key, string(budgetBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{phase3,families,Ethena,spentMicros}','19900000') WHERE route_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	assertBudgetHold(t, admit(), "family_cap_exceeded")
	if err = db.pool.QueryRow(ctx, `SELECT expected_effects ? 'phase3' OR signed_wire IS NOT NULL OR broadcast_intent_at IS NOT NULL FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&unexpected); err != nil || unexpected {
		t.Fatal("rejected producer left an authorization or wire", err)
	}
	// Restore the empty synthetic fixture after the negative case, not a
	// production reset mechanism. The next calls derive every reservation.
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{phase3}',$2::jsonb) WHERE route_key=$1`, key, string(budgetBytes)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- admit() }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	read := func(database *Database) (Phase3Budget, phase3OperationAuthorization) {
		t.Helper()
		var state, authorization []byte
		var hasWire, hasIntent bool
		if err := database.pool.QueryRow(ctx, `SELECT route.state->'phase3',operation.expected_effects->'phase3',operation.signed_wire IS NOT NULL,operation.broadcast_intent_at IS NOT NULL FROM loyal_yield.multiply_route_states route JOIN loyal_yield.multiply_operations operation USING(route_key) WHERE operation_id=$1`, id).Scan(&state, &authorization, &hasWire, &hasIntent); err != nil {
			t.Fatal(err)
		}
		var b Phase3Budget
		var auth phase3OperationAuthorization
		if json.Unmarshal(state, &b) != nil || json.Unmarshal(authorization, &auth) != nil || hasWire || hasIntent {
			t.Fatal("admission signed/sent or corrupted durable evidence")
		}
		return b, auth
	}
	budget, auth := read(db)
	reservation := budget.Reservations[id]
	if len(budget.Reservations) != 1 || auth.BridgeAdmission == nil || len(auth.BridgeAdmission.Exit) != 5 || auth.BuildInput == nil ||
		reservation.UpperMicros != auth.BridgeAdmission.CurrentCost.TotalMicros || reservation.ExitAfterMicros != auth.BridgeAdmission.ExitAfterMicros ||
		budget.Families["Ethena"].ExitMicros != reservation.ExitAfterMicros || budget.Families["Ethena"].SpentMicros != 0 {
		t.Fatal("producer did not atomically reserve its own measured full return graph")
	}
	if err = authorizePhase3ProductionBuild(ctx, db, budgetBuildRPC(t, 5_000, 42), id, evidence.Request, evidence.ExpectedEffects, auth.BuildInput.Effects); err != nil {
		t.Fatal(err)
	}
	staleCost := auth.BridgeAdmission.CurrentCost
	staleCost.ObservationSlot = auth.BridgeAdmission.ValidThroughSlot + 1
	assertBudgetHold(t, db.authorizePhase3Build(ctx, nil, id, evidence.Request, auth.BuildInput.Effects, staleCost), "stale_bridge_admission_snapshot")
	// Restart under a new fence retains producer evidence and exactly one
	// reservation; an old process cannot admit or release its successor's work.
	if _, err = db.ReleaseRouteLease(ctx); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err = restarted.AcquireRouteLease(ctx, key, "bridge-producer-restarted", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err = restarted.admitPhase3Bridge(ctx, budgetBuildRPC(t, 5_000, 42), id, o, d, evidence); err != nil {
		t.Fatal(err)
	}
	after, afterAuth := read(restarted)
	if !reflect.DeepEqual(budget, after) || !reflect.DeepEqual(auth, afterAuth) {
		t.Fatal("restart changed measured admission or replenished budget")
	}
	if err = admit(); err == nil {
		t.Fatal("stale writer retained admission authority")
	}
	changed := evidence
	changed.Request.RecentBlockhash = bridgeSettings
	assertBudgetHold(t, restarted.admitPhase3Bridge(ctx, budgetBuildRPC(t, 5_000, 42), id, o, d, changed), "reservation_identity_mismatch")
}
