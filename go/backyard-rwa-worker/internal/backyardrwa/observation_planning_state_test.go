package backyardrwa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func planningPilotState(t *testing.T) map[string]any {
	t.Helper()
	prior := emptyTestBudget()
	flat := pilotFlatFixture(t)
	previous, err := json.Marshal(prior)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(flat)
	if err != nil {
		t.Fatal(err)
	}
	authority := pilotTestAuthority(prior)
	authority.Generation, authority.FinalizedSlot = 2, flat.Slot
	authority.FlatEvidenceSHA256 = sha256Bytes(encoded)
	budget, err := activatePilotBudget(prior, authority)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"generation": 2, "phase3": budget, "pilotBudgetActivation": pilotBudgetActivation{authority, previous, flat}}
}

func TestRoutePlanningStateSharesValidatedAuthorityAndSelectors(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 20*time.Second)
	defer cancel()
	defer db.Close()
	key := fmt.Sprintf("planning-state-%d", time.Now().UnixNano())
	state := planningPilotState(t)
	entry := selectorEntryFixture(time.Now().UTC(), mapleSyrupUSDCUSDC.Lane, 1_000_000)
	unwind := UnwindIntent{SourceLane: mapleSyrupUSDCUSDC.Lane, Reason: "economic_rotation", ObservationID: "source", MaxCollateralRaw: 100, MaxDebtRaw: 50, CostBoundRaw: 100, BudgetScope: Phase3GoalID, BudgetFamily: "Maple", EvidenceID: sha256Bytes([]byte("exit")), CreatedAt: time.Now().UTC()}
	state["selectorEntry"], state["selectorUnwind"], state["selectorEntryPaused"] = entry, unwind, true
	raw, _ := json.Marshal(state)
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,2)`, key, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, key, "planning-test", time.Minute); err != nil {
		t.Fatal(err)
	}
	planning, err := db.readRoutePlanningState(ctx, key, true)
	if err != nil {
		t.Fatal(err)
	}
	active, baseline, err := db.PilotRuntimeState(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	originalEntry, err := db.LoadSelectorEntry(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	originalUnwind, err := db.LoadUnwindIntent(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !active || planning.pilot != active || !reflect.DeepEqual(planning.baseline, baseline) || !reflect.DeepEqual(planning.entry, originalEntry) || !reflect.DeepEqual(planning.unwind, originalUnwind) || !planning.paused {
		t.Fatalf("planning disagreed with original validators: %+v", planning)
	}
	// No execution lease is granted by the shadow read, even on the same DB.
	readonly := &Database{pool: db.pool}
	if _, err := readonly.readRoutePlanningState(ctx, key, true); !errors.Is(err, ErrRouteLeaseLost) {
		t.Fatalf("production planning read did not require its own lease: %v", err)
	}
	shadow, err := readonly.readRoutePlanningState(ctx, key, false)
	if err != nil || shadow.lease != nil {
		t.Fatalf("shadow acquired authority: %+v %v", shadow, err)
	}
	manifest := planning.observationManifest(RouteManifest{})
	if !manifest.selectorObservation || manifest.observationLane != unwind.SourceLane {
		t.Fatal("unwind did not select the observation lane")
	}
	for _, field := range []string{"pilotBudgetActivation", "selectorEntry", "selectorUnwind"} {
		t.Run(field+" malformed", func(t *testing.T) {
			if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set($2::jsonb,ARRAY[$3]::text[],'{}'::jsonb) WHERE route_key=$1`, key, raw, field); err != nil {
				t.Fatal(err)
			}
			if _, err := db.readRoutePlanningState(ctx, key, true); err == nil {
				t.Fatal("accepted malformed planning authority")
			}
		})
	}
}

// This uses real PostgreSQL and the production observation/projection/Tick
// path. A selector or authority update between planning and the bank read must
// abort before either a projection write or a transaction decision can escape.
func TestRoutePlanningGenerationChangeCannotReachDecision(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 20*time.Second)
	defer cancel()
	defer db.Close()
	for _, mutation := range []string{"selector", "authority", "health_hold"} {
		t.Run(mutation, func(t *testing.T) {
			key := fmt.Sprintf("planning-race-%s-%d", mutation, time.Now().UnixNano())
			if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,'{"generation":1}',1)`, key); err != nil {
				t.Fatal(err)
			}
			if _, err := db.AcquireRouteLease(ctx, key, "planning-race", time.Minute); err != nil {
				t.Fatal(err)
			}
			observation := Observation{ObservedAt: time.Now().UTC(), Snapshot: Snapshot{ObservationID: "allocation", Slot: 42, Fresh: true, RouteKind: RouteKind, VoltrIdleRaw: 7, TotalVaultNAVRaw: 7, ReportSequence: 42, ReportSnapshotDigest: strings.Repeat("a", 64)}}
			if got := Decide(observation.Snapshot); got.Action != VoltrAllocateToSquads {
				t.Fatalf("fixture was not actionable: %+v", got)
			}
			if mutation == "health_hold" {
				observation.Snapshot.ManualReason = "observed_health_fault"
			}
			state := productionObserveState{routeKey: key, manifest: readyWorkerManifest(t), journal: db, identity: pinnedIdentityObservation}
			state.batch = func(ctx context.Context) (Observation, error) {
				planning, err := db.readRoutePlanningState(ctx, key, true)
				if err != nil {
					return Observation{}, err
				}
				changed := map[string]any{"generation": 2, "selectorEntryPaused": true}
				if mutation == "authority" {
					changed = planningPilotState(t)
				}
				raw, err := json.Marshal(changed)
				if err != nil {
					return Observation{}, err
				}
				if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=$2,state_version=2 WHERE route_key=$1`, key, raw); err != nil {
					return Observation{}, err
				}
				observation.planning = planning
				return observation, nil
			}
			escaped := false
			worker := Worker{routeKey: productionRouteKey, manifest: readyWorkerManifest(t), runtime: tickRuntime{
				loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil }, observe: state.observe,
				prepareBridge: func(context.Context, RouteManifest, Decision, Observation) (Observation, BridgeExecutionEvidence, error) {
					escaped = true
					return Observation{}, BridgeExecutionEvidence{}, fmt.Errorf("unexpected construction")
				},
				recordDecision: func(context.Context, string, Observation, Decision, string, string) (DecisionRecord, error) {
					escaped = true
					return DecisionRecord{}, fmt.Errorf("unexpected decision")
				},
			}}
			if err := worker.Tick(ctx); !errors.Is(err, errConfirmedObservationUnavailable) {
				t.Fatalf("generation race did not retry: %v", err)
			}
			var projected bool
			var operations int
			if err := db.pool.QueryRow(ctx, `SELECT state ? 'observation',(SELECT count(*) FROM loyal_yield.multiply_operations WHERE route_key=$1) FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&projected, &operations); err != nil {
				t.Fatal(err)
			}
			if escaped || projected || operations != 0 {
				t.Fatalf("stale planning escaped: construction=%v projection=%v operations=%d", escaped, projected, operations)
			}
		})
	}
}
