package backyard

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
)

func TestWithdrawalHealthPreservesAttentionUntilCurrentRecovery(t *testing.T) {
	t.Parallel()
	o := Observation{ObservedAt: time.Unix(1000, 0).UTC(), Snapshot: livePartialSnapshot()}
	h, ok := assessWithdrawalHealth(rwa, o, Decision{Action: HoldManualRecovery, Reason: "custody_mismatch"}, nil)
	if !ok || h.Status != "operator_attention" {
		t.Fatalf("known blocker: %+v", h)
	}
	h = mergeWithdrawalHealth(nil, h)
	// Serialize/reload exactly like a worker restart; no in-memory onset timer.
	raw, _ := json.Marshal(h)
	var previous earnmax.WithdrawalHealth
	if err := json.Unmarshal(raw, &previous); err != nil {
		t.Fatal(err)
	}
	o.ObservedAt = o.ObservedAt.Add(time.Minute)
	o.Snapshot.Slot++
	for _, d := range []Decision{{Action: ReportNAV}, {Action: RecoverTransaction}, {Action: Hold}} {
		unknown, _ := assessWithdrawalHealth(rwa, o, d, nil)
		kept := mergeWithdrawalHealth(&previous, unknown)
		if kept.Status != "operator_attention" || !kept.ObservedAt.Equal(previous.ObservedAt) || !kept.BlockedSince.Equal(*previous.BlockedSince) {
			t.Fatalf("non-funding evidence erased attention: %+v", kept)
		}
	}
	unknown, _ := assessWithdrawalHealth(rwa, o, Decision{}, errors.New("RPC failed"))
	if kept := mergeWithdrawalHealth(&previous, unknown); kept.Status != "operator_attention" {
		t.Fatal("read error recovered")
	}
	o.Snapshot.VoltrIdleRaw = o.Snapshot.WithdrawalDemandRaw
	covered, _ := assessWithdrawalHealth(rwa, o, Decision{}, nil)
	covered = mergeWithdrawalHealth(&previous, covered)
	if covered.Status != "waiting" || covered.Reason != "withdrawal_covered" || covered.BlockedSince != nil {
		t.Fatalf("covered: %+v", covered)
	}
	o.Snapshot.WithdrawalDemandRaw = 0
	none, _ := assessWithdrawalHealth(rwa, o, Decision{}, nil)
	if none.Status != "none" {
		t.Fatalf("no demand: %+v", none)
	}
	o.Snapshot.Fresh = false
	if _, ok := assessWithdrawalHealth(rwa, o, Decision{}, nil); ok {
		t.Fatal("unknown zero demand became recovery")
	}
}

func TestWithdrawalHealthCurrentHoldBeforeOperationExists(t *testing.T) {
	o := Observation{ObservedAt: time.Now().UTC(), Snapshot: targeted(livePartialSnapshot())}
	var stored withdrawalAssessment
	w := Worker{manifest: rwa.manifest, runtime: tickRuntime{withdrawalHealth: func(_ context.Context, h withdrawalAssessment) error { stored = h; return nil }}}
	// Prepare may return no observation or operation ID. The tick retains its
	// earlier coherent snapshot and projects the current refusal, which the
	// next tick may clear: unavailable, not yet the operator's.
	w.publishWithdrawalHealth(context.Background(), o, Decision{Action: DeleverRouteStep}, roomHold("step_no_withdraw_room", nil))
	if stored.Status != "unavailable" || !stored.ObservedAt.Equal(o.ObservedAt) || stored.ObservedSlot != o.Snapshot.Slot {
		t.Fatalf("pre-operation hold lost: %+v", stored)
	}
	stored = withdrawalAssessment{}
	o.Snapshot.WithdrawalDemandRaw = 0
	w.publishWithdrawalHealth(context.Background(), o, Decision{Action: DeleverRouteStep}, roomHold("step_no_withdraw_room", nil))
	if stored.Status != "none" {
		t.Fatal("unrelated optimizer hold became withdrawal alert")
	}
}

func TestTickPersistsWithdrawalPrepareHoldBeforeRecordingOperation(t *testing.T) {
	o := Observation{ObservedAt: time.Now().UTC(), Snapshot: targeted(livePartialSnapshot())}
	var stored withdrawalAssessment
	recorded := false
	w := Worker{routeKey: productionRouteKey, manifest: readyWorkerManifest(t), runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe:         func(context.Context) (Observation, error) { return o, nil },
		prepareStep: func(context.Context, RouteManifest, Decision, Observation) (Observation, StepExecutionEvidence, error) {
			return Observation{}, StepExecutionEvidence{}, roomHold("step_no_withdraw_room", nil)
		},
		recordDecision: func(context.Context, string, Observation, Decision, string) (DecisionRecord, error) {
			recorded = true
			return DecisionRecord{}, errors.New("must not create operation")
		},
		withdrawalHealth: func(_ context.Context, h withdrawalAssessment) error { stored = h; return nil },
	}}
	err := w.Tick(context.Background())
	var hold *PreSendHold
	if !errors.As(err, &hold) || holdCause(hold) != "step_no_withdraw_room" || recorded || stored.Status != "unavailable" {
		t.Fatalf("pre-operation refusal not projected: err=%v recorded=%v health=%+v", err, recorded, stored)
	}
}

func TestWithdrawalHealthDoesNotObserveDuringRecoveryOrReadFailure(t *testing.T) {
	reads, writes := 0, 0
	w := Worker{manifest: rwa.manifest, runtime: tickRuntime{
		observeWithdrawalHealth: func(context.Context) (Observation, error) {
			reads++
			return Observation{ObservedAt: time.Now().UTC(), Snapshot: livePartialSnapshot()}, nil
		},
		withdrawalHealth: func(ctx context.Context, _ withdrawalAssessment) error {
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
