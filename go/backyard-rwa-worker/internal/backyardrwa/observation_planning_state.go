package backyardrwa

import (
	"context"
	"encoding/json"
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
	pilot      bool
	baseline   *pilotActivationBaseline
	entry      *SelectorEntry
	unwind     *UnwindIntent
	paused     bool
	// remainingExecutionCost is advisory quote-sizing headroom under the
	// reviewed $500 bounded execution-cost stop: the cap less booked spend and
	// every outstanding reservation's cost bound. The binding check stays at
	// reservation time under the record lock; this only shapes the sized quote
	// ladder. Non-pilot states carry the full ceiling.
	remainingExecutionCost int64
}

func (d *Database) readRoutePlanningState(ctx context.Context, routeKey string, execution bool) (*routePlanningState, error) {
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
	var budget, activation, entry, unwind []byte
	err := d.pool.QueryRow(ctx, `SELECT state_version,
		COALESCE(state->'phase3','null'::jsonb),COALESCE(state->'pilotBudgetActivation','null'::jsonb),
		state->'selectorEntry',state->'selectorUnwind',COALESCE((state->>'selectorEntryPaused')::boolean,false)
		FROM loyal_yield.multiply_route_states WHERE route_key=$1
		AND ($2='' OR (lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp()))`,
		routeKey, owner, fence).Scan(&out.generation, &budget, &activation, &entry, &unwind, &out.paused)
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
	out.pilot, out.baseline, err = decodePilotRuntimeState(budget, activation, out.generation)
	if err != nil {
		return nil, err
	}
	out.remainingExecutionCost = int64(PilotEntryExecutionCostCapMicros)
	if out.pilot {
		if out.remainingExecutionCost, err = pilotRemainingExecutionCost(budget); err != nil {
			return nil, err
		}
	}
	out.entry, err = decodeSelectorEntry(entry)
	if err != nil {
		return nil, err
	}
	out.unwind, err = decodeUnwindIntent(unwind)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// pilotRemainingExecutionCost derives the advisory remaining bounded
// entry-cost budget from a validated durable pilot budget: the reviewed
// ceiling less booked execution-cost spend and every outstanding
// reservation's cost bound. The budget is only read here, never mutated.
func pilotRemainingExecutionCost(raw []byte) (int64, error) {
	var pilot Phase3Budget
	if json.Unmarshal(raw, &pilot) != nil || pilot.validate() != nil || pilot.Pilot == nil {
		return 0, budgetHold("invalid_durable_budget")
	}
	spent, err := pilot.executionCostSpent()
	if err != nil {
		return 0, err
	}
	var sumErr error
	for _, reservation := range pilot.Reservations {
		if spent, sumErr = budgetSum(spent, reservation.ExecutionCostUpperMicros); sumErr != nil {
			return 0, sumErr
		}
	}
	remaining := int64(PilotEntryExecutionCostCapMicros) - spent
	if remaining < 0 {
		// Exhausted headroom is a fact, never an unknown: production sizing
		// must fail closed instead of disabling the budget trigger.
		remaining = 0
	}
	return remaining, nil
}

func (p *routePlanningState) observationManifest(manifest RouteManifest) RouteManifest {
	if p.pilot {
		manifest.selectorObservation = true
		if p.entry != nil {
			manifest.observationLane = p.entry.Lane
		}
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
