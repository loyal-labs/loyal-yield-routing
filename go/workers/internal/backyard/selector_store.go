package backyard

import (
	"context"
)

func manifestForUnwind(ctx context.Context, database *Database, manifest RouteManifest) (RouteManifest, error) {
	pilot, err := database.PilotRuntimeEnabled(ctx, productionRouteKey)
	if err != nil {
		return manifest, err
	}
	if pilot {
		// Observe all pilot ownership accounts, including during ordinary entry.
		// Actual exposure always wins over the preferred next destination.
		// Both durable reads resolve their lane authority through the same
		// reviewed manifest, so a persisted candidate entry or candidate-source
		// unwind orients the observation exactly while that binding resolves.
		manifest.selectorObservation = true
		entry, err := database.LoadSelectorEntryOnManifest(ctx, manifest, productionRouteKey)
		if err != nil {
			return manifest, err
		}
		if entry != nil {
			manifest.observationLane = entry.Lane
		}
	}
	intent, err := database.LoadUnwindIntentOnManifest(ctx, manifest, productionRouteKey)
	if err != nil {
		return manifest, err
	}
	if intent != nil {
		manifest.selectorObservation = true
		manifest.observationLane = intent.SourceLane
	}
	return manifest, nil
}
