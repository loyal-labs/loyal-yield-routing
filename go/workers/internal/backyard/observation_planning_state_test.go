package backyard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func planningPilotState(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{"generation": 2}
}

// The planning read carries target()'s stored inputs and nothing else: the
// lane levels, the operator exits and the selector's latest result, under the
// execution lease (a shadow read takes none).
func TestRoutePlanningStateReadsTheTargetInputs(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 20*time.Second)
	defer cancel()
	defer db.Close()
	key := fmt.Sprintf("planning-state-%d", time.Now().UnixNano())
	quote := entryQuoteFixture(time.Now().UTC(), mapleSyrupUSDCUSDC.Lane, 1_000_000)
	state := planningPilotState(t)
	state["selector"] = map[string]any{"result": SelectorResult{Action: "ENTER", DestinationLane: quote.DestinationLane, SelectedQuote: &quote}}
	// Keys of the deleted move machines are ignored if a row still has them.
	state["selectorUnwind"], state["tranche"], state["workingLane"] = map[string]any{"sourceLane": autoAUTOPYUSD.Lane}, map[string]any{"source": "x"}, autoAUTOPYUSD.Lane
	raw, _ := json.Marshal(state)
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,2)`, key, raw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.pool.Exec(context.Background(), `DELETE FROM loyal_yield.backyard_lane_levels WHERE route_key=$1`, key)
		_, _ = db.pool.Exec(context.Background(), `DELETE FROM loyal_yield.backyard_lane_exits WHERE route_key=$1`, key)
	})
	if _, err := db.AcquireRouteLease(ctx, key, "planning-test", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordLaneLevel(ctx, key, autoAUTOPYUSD.Lane, laneLevel{Level: 1.75, UpApproved: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordLaneLevel(ctx, key, autoAUTOWYLDSLane, laneLevel{Level: 1.5}); err != nil {
		t.Fatal(err)
	}
	// An approval with no level to lever toward is no row.
	if err := db.RecordLaneLevel(ctx, key, autoAUTOPYUSD.Lane, laneLevel{Level: 1, UpApproved: true}); err == nil {
		t.Fatal("approved 1x accepted")
	}
	if _, err := db.recordLaneExit(ctx, key, LaneExitRequest{Lane: autoAUTOWYLDSLane, RequestedBy: "operator"}); err != nil {
		t.Fatal(err)
	}
	planning, err := db.readRoutePlanningState(ctx, key, true)
	if err != nil {
		t.Fatal(err)
	}
	in := planning.inputs
	if in.Levels[autoAUTOPYUSD.Lane] != (laneLevel{Level: 1.75, UpApproved: true}) || in.Levels[autoAUTOWYLDSLane] != (laneLevel{Level: 1.5}) || len(in.Levels) != 2 {
		t.Fatalf("levels: %+v", in.Levels)
	}
	if exit, ok := in.Exits[autoAUTOWYLDSLane]; !ok || exit.EquityZero || len(in.Exits) != 1 {
		t.Fatalf("exits: %+v", in.Exits)
	}
	if in.Entry == nil || in.Entry.Action != "ENTER" || in.Entry.SelectedQuote == nil || in.Entry.SelectedQuote.EquityRaw != quote.EquityRaw {
		t.Fatalf("selector result: %+v", in.Entry)
	}
	// Clearing the exit removes the row; equity_zero rewrites it.
	if _, err = db.recordLaneExit(ctx, key, LaneExitRequest{Lane: autoAUTOWYLDSLane, Clear: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.recordLaneExit(ctx, key, LaneExitRequest{Lane: autoAUTOPYUSD.Lane, EquityZero: true, RequestedBy: "operator"}); err != nil {
		t.Fatal(err)
	}
	if planning, err = db.readRoutePlanningState(ctx, key, true); err != nil || len(planning.inputs.Exits) != 1 || !planning.inputs.Exits[autoAUTOPYUSD.Lane].EquityZero {
		t.Fatalf("exits after clear: %+v %v", planning, err)
	}
	// No execution lease is granted by the shadow read, even on the same DB.
	readonly := &Database{pool: db.pool}
	if _, err := readonly.readRoutePlanningState(ctx, key, true); !errors.Is(err, ErrRouteLeaseLost) {
		t.Fatalf("production planning read did not require its own lease: %v", err)
	}
	shadow, err := readonly.readRoutePlanningState(ctx, key, false)
	if err != nil || shadow.lease != nil || len(shadow.inputs.Levels) != 2 {
		t.Fatalf("shadow acquired authority or lost inputs: %+v %v", shadow, err)
	}
	// A level written without the route lease is refused.
	if err = readonly.RecordLaneLevel(ctx, key, autoAUTOPYUSD.Lane, laneLevel{Level: 1.5}); err == nil {
		t.Fatal("level written without the lease")
	}
	// A malformed selector result fails the read instead of entering.
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{selector,result}','"x"'::jsonb) WHERE route_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if _, err = db.readRoutePlanningState(ctx, key, true); err == nil {
		t.Fatal("malformed selector result accepted")
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
			observation := Observation{ObservedAt: time.Now().UTC(), Snapshot: Snapshot{ObservationID: "allocation", Slot: 42, Fresh: true, RouteKind: RouteKind, RouteLane: SelectedRouteID, StrategyKey: SelectedRouteID,
				LiquidationThresholdBPS: 8000, CapacityRaw: 7, PolicyLimitRaw: 7, MaxTargetLTVEntryRaw: 7, VoltrIdleRaw: 7, TotalVaultNAVRaw: 7, ReportSequence: 42, ReportSnapshotDigest: strings.Repeat("a", 64),
				Target: LaneTarget{Rule: ruleEntry, EntryRaw: 7}}}
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
				changed := map[string]any{"generation": 2, "selector": map[string]any{"result": SelectorResult{Action: "KEEP"}}}
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
				recordDecision: func(context.Context, string, Observation, Decision, string) (DecisionRecord, error) {
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
