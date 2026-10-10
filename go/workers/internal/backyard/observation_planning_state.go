package backyard

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// One database snapshot supplies lane selection and planning enrichment for
// one observation. It is never serialized or retained across observations.
// Production rechecks this generation under its projection lock; selector
// collection rechecks it under RecordSelectorEvaluation's existing lock.
type routePlanningState struct {
	routeKey   string
	generation int64
	lease      *RouteLease
	entry      *SelectorEntry
	unwind     *UnwindIntent
	paused     bool
	// leverage is the durable B2 option-1 level target (nil = none stored).
	leverage          *LeverageTarget
	partialWithdrawal *partialWithdrawalState
	// landedSlot is the highest slot any of the route's operations landed at.
	landedSlot int64
}

func (d *Database) readRoutePlanningState(ctx context.Context, routeKey string, execution bool) (*routePlanningState, error) {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return nil, err
	}
	return d.readRoutePlanningStateOnManifest(ctx, manifest, routeKey, execution)
}

// readRoutePlanningStateOnManifest is the identical batch planning read with
// the durable entry and unwind decodes resolved through the explicit reviewed
// manifest: the candidate AUTO entry and a recorded candidate-source unwind
// are decoded through that manifest's lane authority, and every lease and
// generation check is shared verbatim.
func (d *Database) readRoutePlanningStateOnManifest(ctx context.Context, manifest RouteManifest, routeKey string, execution bool) (*routePlanningState, error) {
	if d == nil || d.pool == nil || routeKey == "" {
		return nil, fmt.Errorf("planning state database is not configured")
	}
	out := &routePlanningState{routeKey: routeKey}
	owner := ""
	var fence int64
	if execution {
		lease, err := d.currentLease()
		if err != nil || lease.RouteKey != routeKey {
			return nil, ErrRouteLeaseLost
		}
		out.lease = &lease
		owner, fence = lease.Owner, lease.FencingToken
	}
	var entry, unwind, leverage, partial []byte
	err := d.pool.QueryRow(ctx, `SELECT state_version,
		state->'selectorEntry',state->'selectorUnwind',COALESCE((state->>'selectorEntryPaused')::boolean,false),
		COALESCE(state->'leverageTarget','null'::jsonb),state->'partialWithdrawal',
		(SELECT COALESCE(MAX(confirmed_slot),0) FROM loyal_yield.multiply_operations WHERE route_key=$1)
		FROM loyal_yield.multiply_route_states WHERE route_key=$1
		AND ($2='' OR (lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp()))`,
		routeKey, owner, fence).Scan(&out.generation, &entry, &unwind, &out.paused, &leverage, &partial, &out.landedSlot)
	if errors.Is(err, pgx.ErrNoRows) && execution {
		d.setLease(nil)
		return nil, ErrRouteLeaseLost
	}
	if err != nil {
		return nil, err
	}
	if out.generation <= 0 {
		return nil, fmt.Errorf("invalid planning generation")
	}
	out.entry, err = manifest.decodeSelectorEntry(entry)
	if err != nil {
		return nil, err
	}
	out.unwind, err = manifest.decodeUnwindIntent(unwind)
	if err != nil {
		return nil, err
	}
	out.leverage, err = decodeLeverageTarget(leverage)
	if err != nil {
		return nil, err
	}
	if len(partial) > 0 && string(partial) != "null" {
		if err = d.validatePartialWithdrawalOrigin(ctx, d.pool, routeKey, partial); err != nil {
			return nil, err
		}
	}
	out.partialWithdrawal, err = decodePartialWithdrawal(partial)
	if err != nil || (out.partialWithdrawal != nil && out.partialWithdrawal.Generation > out.generation) {
		return nil, budgetHold("invalid_partial_withdrawal_generation")
	}
	return out, nil
}

func (p *routePlanningState) observationManifest(manifest RouteManifest) RouteManifest {
	manifest.selectorObservation = true
	if p.entry != nil {
		manifest.observationLane = p.entry.Lane
	}
	if p.unwind != nil {
		manifest.selectorObservation = true
		manifest.observationLane = p.unwind.SourceLane
	}
	return manifest
}

func (p *routePlanningState) validateGeneration(routeKey string, lease RouteLease, generation int64) error {
	if p == nil {
		return nil
	}
	if p.routeKey != routeKey || p.lease == nil || p.lease.Owner != lease.Owner || p.lease.FencingToken != lease.FencingToken || p.generation != generation {
		return confirmedObservationUnavailable(fmt.Errorf("route planning state changed during observation"))
	}
	return nil
}

// Health failures do not write a position projection. They still must not
// return a decision made from planning facts superseded during the RPC read.
func (d *Database) validateRoutePlanningState(ctx context.Context, routeKey string, planning *routePlanningState) error {
	lease, err := d.currentLease()
	if err != nil {
		return err
	}
	var generation int64
	if err = d.pool.QueryRow(ctx, `SELECT state_version FROM loyal_yield.multiply_route_states WHERE route_key=$1 AND lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp()`, routeKey, lease.Owner, lease.FencingToken).Scan(&generation); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRouteLeaseLost
		}
		return err
	}
	return planning.validateGeneration(routeKey, lease, generation)
}
