package backyard

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The stored inputs of target(), and nothing else. The withdrawal queue is
// Voltr's own receipts, read with the chain. The two rows here are:
//
//   - a lane's level (loyal_yield.backyard_lane_levels): the leverage the
//     selector's economics chose for it, and whether the up step toward it is
//     worth its cost at the last sample;
//   - an operator exit (loyal_yield.backyard_lane_exits): "lane X target debt
//     0", optionally with its collateral returned to Voltr too.

// laneLevel is one lane's stored level.
type laneLevel struct {
	Level      float64 `json:"level"`
	UpApproved bool    `json:"upApproved,omitempty"`
}

// laneExit is one lane's operator exit.
type laneExit struct {
	EquityZero bool `json:"equityZero,omitempty"`
}

// targetInputs is everything target() reads besides the chain.
type targetInputs struct {
	Levels map[string]laneLevel
	Exits  map[string]laneExit
	// Entry is the selector's latest result, the rates' choice of a lane for a
	// flat vault's cash; nil when the selector has not written one.
	Entry *SelectorResult
}

func validLevel(level float64) bool {
	for _, l := range leverageLevels {
		if level == l {
			return true
		}
	}
	return false
}

// decodeTargetInputs decodes the planning read's level and exit aggregates.
func decodeTargetInputs(levels, exits, entry []byte) (targetInputs, error) {
	var in targetInputs
	if err := json.Unmarshal(levels, &in.Levels); err != nil {
		return in, budgetHold("invalid_lane_levels")
	}
	// A row for a lane this binary does not install has no view to target;
	// it never stops the lanes that are installed (hard LTV included).
	for lane, level := range in.Levels {
		if !earnHeldLane(lane) {
			delete(in.Levels, lane)
		} else if !validLevel(level.Level) {
			return in, budgetHold("invalid_lane_levels")
		}
	}
	if err := json.Unmarshal(exits, &in.Exits); err != nil {
		return in, budgetHold("invalid_lane_exits")
	}
	for lane := range in.Exits {
		if !earnHeldLane(lane) {
			delete(in.Exits, lane)
		}
	}
	if len(entry) > 0 && string(entry) != "null" {
		var result SelectorResult
		if json.Unmarshal(entry, &result) != nil {
			return in, budgetHold("invalid_selector_result")
		}
		in.Entry = &result
	}
	return in, nil
}

// targetInputsSQL reads the levels and exits of route $1 as two JSON objects
// keyed by lane.
const targetInputsSQL = `COALESCE((SELECT jsonb_object_agg(lane, jsonb_build_object('level', level, 'upApproved', up_approved)) FROM loyal_yield.backyard_lane_levels WHERE route_key = $1), '{}'::jsonb),
	COALESCE((SELECT jsonb_object_agg(lane, jsonb_build_object('equityZero', equity_zero)) FROM loyal_yield.backyard_lane_exits WHERE route_key = $1), '{}'::jsonb)`

// RecordLaneLevel stores one lane's level. The selector sample loop is its
// only writer; it changes no money by itself.
func (d *Database) RecordLaneLevel(ctx context.Context, routeKey, lane string, level laneLevel) error {
	if !earnActiveLane(lane) || !validLevel(level.Level) || (level.UpApproved && level.Level <= 1) {
		return fmt.Errorf("invalid_lane_level")
	}
	_, err := d.pool.Exec(ctx, `INSERT INTO loyal_yield.backyard_lane_levels (route_key, lane, level, up_approved, decided_at)
		VALUES ($1, $2, $3, $4, clock_timestamp())
		ON CONFLICT (route_key, lane) DO UPDATE SET level = EXCLUDED.level, up_approved = EXCLUDED.up_approved, decided_at = EXCLUDED.decided_at`,
		routeKey, lane, level.Level, level.UpApproved)
	return err
}

// LaneExitRequest is one `loyal-engine backyard exit-lane` call.
type LaneExitRequest struct {
	Lane        string
	EquityZero  bool
	Clear       bool
	RequestedBy string
}

// RunLaneExit writes or clears one lane's operator exit row. Writing it sets
// the lane's target debt to 0 (and, with EquityZero, its equity too): the
// worker delevers it by atomic down steps on its next ticks.
func RunLaneExit(ctx context.Context, databaseURL string, request LaneExitRequest) (string, error) {
	if !earnHeldLane(request.Lane) || (!request.Clear && (request.RequestedBy == "" || len(request.RequestedBy) > 256)) {
		return "", fmt.Errorf("exit-lane: a registry lane and --by <operator> (at most 256 characters) are required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	database, err := openOperatorDatabase(ctx, databaseURL)
	if err != nil {
		return "", err
	}
	defer database.Close()
	return database.recordLaneExit(ctx, productionRouteKey, request)
}

func (d *Database) recordLaneExit(ctx context.Context, routeKey string, request LaneExitRequest) (string, error) {
	if request.Clear {
		tag, err := d.pool.Exec(ctx, `DELETE FROM loyal_yield.backyard_lane_exits WHERE route_key = $1 AND lane = $2`, routeKey, request.Lane)
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Sprintf("lane %s had no exit", request.Lane), nil
		}
		return fmt.Sprintf("lane %s exit cleared", request.Lane), nil
	}
	var at time.Time
	err := d.pool.QueryRow(ctx, `INSERT INTO loyal_yield.backyard_lane_exits (route_key, lane, equity_zero, requested_by)
		SELECT route_key, $2, $3, $4 FROM loyal_yield.multiply_route_states WHERE route_key = $1
		ON CONFLICT (route_key, lane) DO UPDATE SET equity_zero = EXCLUDED.equity_zero, requested_by = EXCLUDED.requested_by, requested_at = clock_timestamp()
		RETURNING requested_at`, routeKey, request.Lane, request.EquityZero, request.RequestedBy).Scan(&at)
	if err == pgx.ErrNoRows {
		return "", fmt.Errorf("exit-lane: route %s is not provisioned", routeKey)
	}
	if err != nil {
		return "", err
	}
	what := "debt 0"
	if request.EquityZero {
		what = "debt 0 and equity 0"
	}
	return fmt.Sprintf("lane %s target %s from %s", request.Lane, what, at.UTC().Format(time.RFC3339)), nil
}
