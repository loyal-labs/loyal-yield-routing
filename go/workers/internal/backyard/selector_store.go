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
	return manifest, nil
}
