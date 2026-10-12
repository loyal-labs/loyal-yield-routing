package backyard

import (
	"context"
	"fmt"
)

// One database snapshot supplies target()'s stored inputs for one
// observation. It is never serialized or retained across observations.
type routePlanningState struct {
	routeKey string
	inputs   targetInputs
	// landedSlot is the highest slot any of the route's operations landed at.
	landedSlot int64
}

// readRoutePlanningState reads target()'s stored inputs and the selector's
// latest result in one statement.
func (d *Database) readRoutePlanningState(ctx context.Context, routeKey string) (*routePlanningState, error) {
	if d == nil || d.pool == nil || routeKey == "" {
		return nil, fmt.Errorf("planning state database is not configured")
	}
	out := &routePlanningState{routeKey: routeKey}
	var levels, exits, entry []byte
	err := d.pool.QueryRow(ctx, `SELECT COALESCE(state->'selector'->'result','null'::jsonb),
		(SELECT COALESCE(MAX(confirmed_slot),0) FROM loyal_yield.multiply_operations WHERE route_key=$1),
		`+targetInputsSQL+`
		FROM loyal_yield.multiply_route_states WHERE route_key=$1`,
		routeKey).Scan(&entry, &out.landedSlot, &levels, &exits)
	if err != nil {
		return nil, err
	}
	if out.inputs, err = decodeTargetInputs(levels, exits, entry); err != nil {
		return nil, err
	}
	return out, nil
}
