package backyard

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// One database snapshot supplies target()'s stored inputs for one
// observation. It is never serialized or retained across observations.
// Production rechecks this generation under its projection lock; selector
// collection rechecks it under RecordSelectorEvaluation's existing lock.
type routePlanningState struct {
	routeKey   string
	generation int64
	lease      *RouteLease
	inputs     targetInputs
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

// readRoutePlanningStateOnManifest reads the route generation, target()'s
// stored inputs and the selector's latest result in one statement, under the
// execution lease when execution is set.
func (d *Database) readRoutePlanningStateOnManifest(ctx context.Context, _ RouteManifest, routeKey string, execution bool) (*routePlanningState, error) {
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
	var levels, exits, entry []byte
	err := d.pool.QueryRow(ctx, `SELECT state_version, COALESCE(state->'selector'->'result','null'::jsonb),
		(SELECT COALESCE(MAX(confirmed_slot),0) FROM loyal_yield.multiply_operations WHERE route_key=$1),
		`+targetInputsSQL+`
		FROM loyal_yield.multiply_route_states WHERE route_key=$1
		AND ($2='' OR (lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp()))`,
		routeKey, owner, fence).Scan(&out.generation, &entry, &out.landedSlot, &levels, &exits)
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
	if out.inputs, err = decodeTargetInputs(levels, exits, entry); err != nil {
		return nil, err
	}
	return out, nil
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
