package backyardrwa

import (
	"context"
	"encoding/json"
)

// Planning reads the same archived authority checked again by admission/build/
// send. The environment, manifest, opportunity feed and a missing budget cannot
// enable pilot sizing. This read never creates or repairs a budget.
func (d *Database) PilotRuntimeEnabled(ctx context.Context, routeKey string) (bool, error) {
	if d == nil || d.pool == nil {
		return false, budgetHold("pilot_runtime_database_unavailable")
	}
	var raw, marker []byte
	var version int64
	if err := d.pool.QueryRow(ctx, `SELECT COALESCE(state->'phase3','null'::jsonb),COALESCE(state->'pilotBudgetActivation','null'::jsonb),state_version FROM loyal_yield.multiply_route_states WHERE route_key=$1`, routeKey).Scan(&raw, &marker, &version); err != nil {
		return false, err
	}
	if string(raw) == "null" {
		if string(marker) != "null" {
			return false, budgetHold("pilot_marker_without_budget")
		}
		return false, nil
	}
	var b Phase3Budget
	if json.Unmarshal(raw, &b) != nil {
		return false, budgetHold("invalid_durable_budget")
	}
	if err := b.validate(); err != nil {
		return false, err
	}
	if b.Pilot == nil {
		if string(marker) != "null" {
			return false, budgetHold("pilot_marker_without_authority")
		}
		return false, nil
	}
	if _, err := validatePersistedPilotActivation(b, marker, version); err != nil {
		return false, err
	}
	return !b.Closed, nil
}

func workingTrancheCap(s Snapshot) int64 {
	if s.PilotActive && selectorLane(s.RouteLane) {
		return PilotWorkingTrancheCapRaw
	}
	return Phase3WorkingTrancheCapRaw
}
