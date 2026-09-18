package backyardrwa

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
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
	effects, _, afterSquads, err := bridgeExpectedEffects(d, uint64(idle), uint64(strategy), uint64(squads))
	if err != nil {
		t.Fatal(err)
	}
	r.Report.NAVAfterRaw = afterSquads
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
		_, err := legacyAdmissionCostCheck(observePhase3BridgeAdmission(context.Background(), budgetBuildRPC(t, 5_000, 42), o, d, evidence))
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

// Staging is the admission path that empties Squads custody, so every staged
// template must report the drained Squads vault as NAV (zero) and never the
// amount parked in the strategy ATA: Voltr tracks strategy custody separately.
func TestBridgeAdmissionStageTemplatesReportDrainedSquadsNAV(t *testing.T) {
	o, d, evidence := bridgeAdmissionFixture(t, StageSquadsToVoltr, 999_952, 214_944, 0, 999_952)
	steps, err := phase3BridgeTemplates(o.Snapshot, d, evidence)
	if err != nil {
		t.Fatal(err)
	}
	var actions []Action
	for _, step := range steps {
		actions = append(actions, step.Request.Action)
		if step.Request.Report.NAVAfterRaw != 0 {
			t.Fatalf("step %s counted strategy custody in NAV: %d", step.Request.Action, step.Request.Report.NAVAfterRaw)
		}
	}
	if !reflect.DeepEqual(actions, []Action{StageSquadsToVoltr, ReportNAV, VoltrRestoreIdle, ReportNAV}) {
		t.Fatalf("unexpected staged exit graph: %v", actions)
	}
	if steps[0].Request.AmountRaw != 999_952 || steps[2].Request.AmountRaw != 999_952 {
		t.Fatalf("stage/restore lost the full custody amounts: %d/%d",
			steps[0].Request.AmountRaw, steps[2].Request.AmountRaw)
	}
	// A request still carrying the pre-staging NAV contradicts the compiled
	// poststate and must be refused at the same intent-mismatch boundary.
	evidence.Request.Report.NAVAfterRaw = 999_952
	_, err = phase3BridgeTemplates(o.Snapshot, d, evidence)
	assertBudgetHold(t, err, "bridge_admission_intent_mismatch")
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

// Mirrors the measured-plan check at the locked production admission boundary.
// The observer itself has no authority to select a deployment's budget.
func legacyAdmissionCostCheck(plan phase3BridgeAdmission, err error) (phase3BridgeAdmission, error) {
	if err == nil {
		err = emptyTestBudget().validateExitPlanCaps(plan)
	}
	return plan, err
}

// Hold all eight independent valuation reads at a barrier. A serialized
// implementation cannot finish; no timing threshold or live RPC is involved.
func TestBridgeAdmissionReadsIndependentValuationsTogether(t *testing.T) {
	o, d, evidence := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 100_000, 200_000, 0, 0)
	rpc := budgetBuildRPC(t, 5_000, 42)
	base := rpc.client.Transport
	var started atomic.Int32
	ready := make(chan struct{})
	rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		var call struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &call); err != nil {
			return nil, err
		}
		if call.Method == "getFeeForMessage" || call.Method == "getMultipleAccounts" {
			var options struct {
				MinimumSlot int64 `json:"minContextSlot"`
			}
			if err := json.Unmarshal(call.Params[1], &options); err != nil {
				return nil, err
			}
			if options.MinimumSlot != 42 {
				return nil, fmt.Errorf("unexpected minimum slot %d", options.MinimumSlot)
			}
			if started.Add(1) == 8 {
				close(ready)
			}
			select {
			case <-ready:
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
		}
		return base.RoundTrip(request)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	plan, err := observePhase3BridgeAdmission(ctx, rpc, o, d, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if started.Load() != 8 || len(plan.Exit) != 5 || plan.ValidThroughSlot != 74 {
		t.Fatalf("incomplete concurrent admission: reads=%d exits=%d validity=%d", started.Load(), len(plan.Exit), plan.ValidThroughSlot)
	}
}

func TestBridgeAdmissionConcurrentReadsKeepEveryFreshnessBound(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		feeSlot, priceSlot, finalSlot int64
		nullFee                       bool
		hold                          string
	}{
		{"different fresh slots retain oldest fee", 42, 70, 74, false, ""},
		{"future fee rejected", 70, 42, 60, false, "fee_message_or_slot_mismatch"},
		{"future price rejected", 42, 70, 60, false, "missing_stale_or_mismatched_usdc_valuation"},
		{"expired snapshot rejected", 42, 70, 75, false, "stale_bridge_admission_snapshot"},
		{"null fee rejected", 42, 42, 42, true, "network_fee_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, d, evidence := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 100_000, 200_000, 0, 0)
			rpc := budgetBuildRPC(t, 5_000, tc.finalSlot)
			base := rpc.client.Transport
			rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				request.Body = io.NopCloser(bytes.NewReader(body))
				var call struct {
					Method string `json:"method"`
				}
				if err := json.Unmarshal(body, &call); err != nil {
					return nil, err
				}
				res, err := base.RoundTrip(request)
				if err != nil {
					return nil, err
				}
				if call.Method != "getFeeForMessage" && call.Method != "getMultipleAccounts" {
					return res, nil
				}
				defer res.Body.Close()
				var payload map[string]any
				if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
					return nil, err
				}
				result := payload["result"].(map[string]any)
				slot := tc.priceSlot
				if call.Method == "getFeeForMessage" {
					slot = tc.feeSlot
					if tc.nullFee {
						result["value"] = nil
					}
				}
				result["context"].(map[string]any)["slot"] = slot
				encoded, err := json.Marshal(payload)
				if err != nil {
					return nil, err
				}
				return response(string(encoded)), nil
			})
			plan, err := observePhase3BridgeAdmission(context.Background(), rpc, o, d, evidence)
			if tc.hold != "" {
				assertBudgetHold(t, err, tc.hold)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if plan.ValidThroughSlot != 74 {
				t.Fatalf("newer price extended old fee validity: %d", plan.ValidThroughSlot)
			}
			for _, cost := range append([]phase3BridgeExitCost{{Cost: plan.CurrentCost}}, plan.Exit...) {
				if cost.Cost.ObservationSlot != tc.finalSlot || cost.Cost.Fee.Slot != tc.feeSlot || cost.Cost.NativePrice.ObservedSlot != tc.priceSlot || cost.Cost.ValidThroughSlot != 74 {
					t.Fatalf("lost independently observed slot: %+v", cost.Cost)
				}
			}
		})
	}
}
