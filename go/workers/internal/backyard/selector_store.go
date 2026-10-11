package backyard

import (
	"context"
)

func manifestForUnwind(ctx context.Context, database *Database, manifest RouteManifest) (RouteManifest, error) {
	// Observe all pilot ownership accounts, including during ordinary entry.
	// Actual exposure always wins over the preferred next destination.
	manifest.selectorObservation = true
	entry, err := database.LoadSelectorEntry(ctx, productionRouteKey)
	if err != nil {
		return manifest, err
	}
	if entry != nil {
		manifest.observationLane = entry.Lane
	}
	intent, err := database.LoadUnwindIntent(ctx, productionRouteKey)
	if err != nil {
		return manifest, err
	}
	if intent != nil {
		manifest.selectorObservation = true
		manifest.observationLane = intent.SourceLane
	}
	// A construction refresh observes the same working lane as its tick.
	manifest.workingLane, err = database.LoadWorkingLane(ctx, productionRouteKey)
	return manifest, err
}

// LoadWorkingLane reads the durable working lane, "" when none is recorded.
func (d *Database) LoadWorkingLane(ctx context.Context, routeKey string) (string, error) {
	var lane string
	if err := d.pool.QueryRow(ctx, `SELECT COALESCE(state->>'workingLane','') FROM loyal_yield.multiply_route_states WHERE route_key=$1`, routeKey).Scan(&lane); err != nil {
		return "", err
	}
	if lane != "" && !earnHeldLane(lane) {
		return "", budgetHold("invalid_durable_working_lane")
	}
	return lane, nil
}
