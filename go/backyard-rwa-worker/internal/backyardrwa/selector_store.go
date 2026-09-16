package backyardrwa

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Keep shadow persistence beside the existing route state. It owns neither
// holdings nor budget; replacing this projection cannot reset reservations.
func (d *Database) RecordSelectorShadow(ctx context.Context, routeKey string, observation Observation, markets []LaneEconomics) (SelectorResult, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	// An unresolved transaction is not a coherent economic sample. Preserve
	// previous evidence without refreshing it; MaxSampleGap is enforced on the
	// next sample. In particular, routine NAV reports must not erase history.
	if observation.Snapshot.Nonterminal != "" || observation.Snapshot.HasAmbiguousSubmission {
		return SelectorResult{Action: "KEEP", Reason: "recover_transaction_before_sampling"}, nil
	}
	lease, err := d.currentLease()
	if err != nil {
		return SelectorResult{}, err
	}
	if lease.RouteKey != routeKey {
		return SelectorResult{}, fmt.Errorf("shadow_route_lease_mismatch")
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return SelectorResult{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout = '25ms'`); err != nil {
		return SelectorResult{}, err
	}
	var version int64
	var raw []byte
	if err = tx.QueryRow(ctx, RouteStateForUpdate, routeKey, lease.Owner, lease.FencingToken).Scan(&version, &raw); err != nil {
		return SelectorResult{}, err
	}
	var pending bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key=$1 AND status IN (`+nonterminalStatusSQL+`))`, routeKey).Scan(&pending); err != nil {
		return SelectorResult{}, err
	}
	if pending {
		return SelectorResult{Action: "KEEP", Reason: "recover_transaction_before_sampling"}, nil
	}
	var previous struct {
		Selector struct {
			Result SelectorResult `json:"result"`
		} `json:"selector"`
	}
	if err = json.Unmarshal(raw, &previous); err != nil {
		return SelectorResult{}, err
	}
	result := SelectOpportunity(SelectorInput{Now: time.Now().UTC(), Snapshot: observation.Snapshot, Markets: markets, Policy: DefaultSelectorPolicy()}, previous.Selector.Result.State)
	encoded, err := json.Marshal(struct {
		Mode          string         `json:"mode"`
		ObservationID string         `json:"observationId"`
		Slot          int64          `json:"slot"`
		Result        SelectorResult `json:"result"`
	}{"shadow", observation.Snapshot.ObservationID, observation.Snapshot.Slot, result})
	if err != nil {
		return SelectorResult{}, err
	}
	tag, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{selector}',$5::jsonb,true),updated_at=clock_timestamp() WHERE route_key=$1 AND lease_owner=$2 AND fencing_token=$3 AND state_version=$4 AND lease_expires_at>clock_timestamp()`, routeKey, lease.Owner, lease.FencingToken, version, string(encoded))
	if err != nil {
		return SelectorResult{}, err
	}
	if tag.RowsAffected() != 1 {
		return SelectorResult{}, fmt.Errorf("shadow_write_lost_route_fence")
	}
	return result, tx.Commit(ctx)
}

func manifestForUnwind(ctx context.Context, database *Database, manifest RouteManifest) (RouteManifest, error) {
	pilot, err := database.PilotRuntimeEnabled(ctx, productionRouteKey)
	if err != nil {
		return manifest, err
	}
	if pilot {
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
