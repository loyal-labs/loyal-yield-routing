package backyard

import (
	"context"
	"encoding/json"
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
	var levels, exits, entry, costs, fee []byte
	var unresolved bool
	err := d.pool.QueryRow(ctx, `SELECT COALESCE(state->'selector'->'result','null'::jsonb),
		(SELECT COALESCE(MAX(confirmed_slot),0) FROM loyal_yield.multiply_operations WHERE route_key=$1),
		`+targetInputsSQL+`, state->'performanceFee', CASE WHEN $1='backyard:6HTfVjwk5FfW4GH67hmEen7VHeURdB5EJj6E429qTrUY' THEN `+unresolvedFeeSQL+` ELSE false END
		FROM loyal_yield.multiply_route_states WHERE route_key=$1`,
		routeKey).Scan(&entry, &out.landedSlot, &levels, &exits, &costs, &fee, &unresolved)
	if err != nil {
		return nil, err
	}
	if out.inputs, err = decodeTargetInputs(levels, exits, entry, costs); err != nil {
		return nil, err
	}
	if unresolved {
		return nil, staleHold("looping_fee_outcome_unresolved")
	}
	if routeKey == looping.RouteKey {
		var last []byte
		var delta int64
		// The one-writer journal has every finalized strategy cash movement.
		// A positive residual at the next publication is outside capital.
		err = d.pool.QueryRow(ctx, `WITH last AS (SELECT confirmed_slot, expected_effects #> '{expectedEffects,performanceFee,after}' AS state
            FROM loyal_yield.multiply_operations WHERE route_key=$1 AND status='reconciled'
              AND expected_effects #> '{expectedEffects,performanceFee}' IS NOT NULL ORDER BY confirmed_slot DESC LIMIT 1)
            SELECT (SELECT state FROM last), COALESCE((SELECT sum((reconciled_effects->>'loopingCashDelta')::bigint)
              FROM loyal_yield.multiply_operations WHERE route_key=$1 AND status='reconciled' AND reconciled_effects ? 'loopingCashDelta'
                AND confirmed_slot > (SELECT confirmed_slot FROM last)),0)`, routeKey).Scan(&last, &delta)
		if err != nil {
			return nil, err
		}
		if fee != nil || last != nil {
			var f, finalized performanceFeeState
			if json.Unmarshal(fee, &f) != nil || !f.valid() || (last != nil) != (f.Version > 0) ||
				last != nil && (json.Unmarshal(last, &finalized) != nil || finalized != f) {
				return nil, staleHold("invalid_looping_fee_checkpoint")
			}
			out.inputs.performanceFee = &f
			out.inputs.performanceCashDelta = delta
		}
	}
	return out, nil
}
