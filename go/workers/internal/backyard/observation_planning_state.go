package backyard

import (
	"context"
	"fmt"
)

type routePlanningState struct {
	routeKey   string
	inputs     targetInputs
	landedSlot int64
}

func (d *Database) readRoutePlanningState(ctx context.Context, routeKey string) (*routePlanningState, error) {
	if d == nil || d.pool == nil || routeKey == "" {
		return nil, fmt.Errorf("planning state database is not configured")
	}
	out := &routePlanningState{routeKey: routeKey}
	var levels, exits, entry, costs []byte
	err := d.pool.QueryRow(ctx, `SELECT COALESCE(state->'selector'->'result','null'::jsonb),
		(SELECT COALESCE(MAX(confirmed_slot),0) FROM loyal_yield.multiply_operations WHERE route_key=$1),
		`+targetInputsSQL+`
		FROM loyal_yield.multiply_route_states WHERE route_key=$1`,
		routeKey).Scan(&entry, &out.landedSlot, &levels, &exits, &costs)
	if err != nil {
		return nil, err
	}
	if out.inputs, err = decodeTargetInputs(levels, exits, entry, costs); err != nil {
		return nil, err
	}
	return out, nil
}
