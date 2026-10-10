package backyard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
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
	plan, err := observePhase3BridgeAdmission(context.Background(), budgetBuildRPC(t, 5_000, 42), budgetView(t), o, d, evidence)
	if err != nil {
		t.Fatal(err)
	}
	var actions []Action
	var total, principal int64
	for _, exit := range plan.Exit {
		actions = append(actions, exit.Action)
		total += exit.Cost.TotalMicros
		principal += exit.Cost.PrincipalMicros
		if exit.Cost.NetworkFeeMicros <= 0 {
			t.Fatal("exit dropped a fee")
		}
	}
	if !reflect.DeepEqual(actions, []Action{ReportNAV, StageSquadsToVoltr, ReportNAV, VoltrRestoreIdle, ReportNAV}) ||
		total != plan.ExitAfterMicros || principal != 2*plan.CurrentCost.PrincipalMicros || plan.Input == nil {
		t.Fatalf("incomplete measured return graph: %+v", plan)
	}
}

func TestBridgeAdmissionRejectsUnpricedExposureAndPartialSweep(t *testing.T) {
	t.Run("position needs complete exit", func(t *testing.T) {
		o, d, evidence := bridgeAdmissionFixture(t, ReportNAV, 0, 0, 0, 0)
		o.Snapshot.PositionDebtRaw = 1
		_, err := observePhase3BridgeAdmission(context.Background(), nil, nil, o, d, evidence)
		assertBudgetHold(t, err, "complete_position_exit_admission_unavailable")
	})
	t.Run("partial restore", func(t *testing.T) {
		o, d, evidence := bridgeAdmissionFixture(t, VoltrRestoreIdle, 1, 0, 100_000, 0)
		_, err := observePhase3BridgeAdmission(context.Background(), nil, nil, o, d, evidence)
		assertBudgetHold(t, err, "bridge_admission_requires_full_custody_exit")
	})
	t.Run("custody effect mismatch", func(t *testing.T) {
		o, d, evidence := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 1, 2, 0, 0)
		evidence.ExpectedEffects.Accounts[0].BeforeRaw++
		_, err := observePhase3BridgeAdmission(context.Background(), nil, nil, o, d, evidence)
		assertBudgetHold(t, err, "bridge_admission_intent_mismatch")
	})
	t.Run("stale snapshot", func(t *testing.T) {
		o, d, evidence := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 1, 2, 0, 0)
		rpc := budgetBuildRPC(t, 5_000, 75)
		_, err := observePhase3BridgeAdmission(context.Background(), rpc, fixtureView(t, rpc), o, d, evidence)
		assertBudgetHold(t, err, "stale_bridge_admission_snapshot")
	})
	t.Run("existing bridge custody cannot be new allocation", func(t *testing.T) {
		o, d, evidence := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 1, 2, 1, 0)
		_, err := observePhase3BridgeAdmission(context.Background(), nil, nil, o, d, evidence)
		assertBudgetHold(t, err, "bridge_allocation_requires_empty_strategy_custody")
	})
}

// Staging is the admission path that empties Squads custody, so every staged
// template must report the drained Squads vault as NAV (zero) and never the
// amount parked in the strategy ATA: Voltr tracks strategy custody separately.
func TestBridgeAdmissionStageTemplatesReportDrainedSquadsNAV(t *testing.T) {
	o, d, evidence := bridgeAdmissionFixture(t, StageSquadsToVoltr, 999_952, 214_944, 0, 999_952)
	steps, err := phase3BridgeTemplates(testPolicies(t), o.Snapshot, d, evidence)
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
	_, err = phase3BridgeTemplates(testPolicies(t), o.Snapshot, d, evidence)
	assertBudgetHold(t, err, "bridge_admission_intent_mismatch")
}

// Hold all eight independent valuation reads at a barrier. A serialized
// implementation cannot finish; no timing threshold or live RPC is involved.
func TestBridgeAdmissionReadsIndependentValuationsTogether(t *testing.T) {
	o, d, evidence := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 100_000, 200_000, 0, 0)
	rpc := budgetBuildRPC(t, 5_000, 42)
	view := fixtureView(t, rpc)
	base := rpcOf(rpc).Transport
	var started atomic.Int32
	ready := make(chan struct{})
	rpcOf(rpc).Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
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
		if call.Method == "getFeeForMessage" {
			var options struct {
				MinimumSlot int64 `json:"minContextSlot"`
			}
			if err := json.Unmarshal(call.Params[1], &options); err != nil {
				return nil, err
			}
			if options.MinimumSlot != 42 {
				return nil, fmt.Errorf("unexpected minimum slot %d", options.MinimumSlot)
			}
			if started.Add(1) == 6 {
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
	plan, err := observePhase3BridgeAdmission(ctx, rpc, view, o, d, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if started.Load() != 6 || len(plan.Exit) != 5 || plan.ValidThroughSlot != 74 {
		t.Fatalf("incomplete concurrent admission: reads=%d exits=%d validity=%d", started.Load(), len(plan.Exit), plan.ValidThroughSlot)
	}
}

// Live 2026-09-28 13:33-13:41: every AUTO whole-debt repayment expired before
// send (send_valuation_expired). The payoff admission prices its exit plan's
// NAV report fee through the bridge admission, whose window was capped at the
// adaptor's 32-slot report age (~8.6 s), while a PYUSD-spending tick needs
// ~12 s from snapshot to send. That report template is never sent as priced,
// so only real report wires keep the 32-slot cap.
func TestReportTemplateAdmissionUsesObservationWindow(t *testing.T) {
	observationLag.Store(49)
	t.Cleanup(func() { observationLag.Store(0) })
	o, d, evidence := bridgeAdmissionFixture(t, ReportNAV, 0, 200_000, 0, 0)
	real, err := observePhase3BridgeAdmission(context.Background(), budgetBuildRPC(t, 5_000, 42), budgetView(t), o, d, evidence)
	if err != nil {
		t.Fatal(err)
	}
	template, err := observePhase3BridgeTemplateAdmission(context.Background(), budgetBuildRPC(t, 5_000, 42), budgetView(t), o, d, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if real.ValidThroughSlot > o.Snapshot.Slot+32 {
		t.Fatalf("a real report wire must stay within the adaptor's 32 slots, got %d", real.ValidThroughSlot-o.Snapshot.Slot)
	}
	if template.ValidThroughSlot <= real.ValidThroughSlot {
		t.Fatalf("the fee-only report template must use the wider observation window: template %d, real %d",
			template.ValidThroughSlot-o.Snapshot.Slot, real.ValidThroughSlot-o.Snapshot.Slot)
	}
}
