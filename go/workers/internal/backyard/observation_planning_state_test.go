package backyard

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func planningPilotState(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{"generation": 2}
}

// The planning read carries target()'s stored inputs and nothing else: the
// lane levels, the operator exits and the selector's latest result.
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
	planning, err := db.readRoutePlanningState(ctx, key)
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
	if planning, err = db.readRoutePlanningState(ctx, key); err != nil || len(planning.inputs.Exits) != 1 || !planning.inputs.Exits[autoAUTOPYUSD.Lane].EquityZero {
		t.Fatalf("exits after clear: %+v %v", planning, err)
	}
	// A malformed selector result fails the read instead of entering.
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{selector,result}','"x"'::jsonb) WHERE route_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if _, err = db.readRoutePlanningState(ctx, key); err == nil {
		t.Fatal("malformed selector result accepted")
	}
}
