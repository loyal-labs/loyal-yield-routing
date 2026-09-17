package backyardrwa

import (
	"context"
	"encoding/json"
	"math"
)

// pilotActivationBaseline is the ticket bookkeeping fact archived by an active
// validated pilot activation: the operator cleanup consumed the report ticket
// to reach the finalized flat baseline, and the activation's flat
// evidence records that consumed sequence. It is not a journal row, it is not
// a NAV report, and it never explains any sequence other than its own.
type pilotActivationBaseline struct {
	TicketLastConsumedSequenceRaw int64
	FinalizedSlot                 int64
}

// Planning reads the same archived authority checked again by admission/build/
// send. The environment, manifest, opportunity feed and a missing budget cannot
// enable pilot sizing. This read never creates or repairs a budget. One
// production read returns the active flag plus, when active and validated, the
// activation baseline (nil when inactive or closed); PilotRuntimeEnabled keeps
// the bool interface on the same single read.
func (d *Database) PilotRuntimeState(ctx context.Context, routeKey string) (bool, *pilotActivationBaseline, error) {
	if d == nil || d.pool == nil {
		return false, nil, budgetHold("pilot_runtime_database_unavailable")
	}
	var raw, marker []byte
	var version int64
	if err := d.pool.QueryRow(ctx, `SELECT COALESCE(state->'phase3','null'::jsonb),COALESCE(state->'pilotBudgetActivation','null'::jsonb),state_version FROM loyal_yield.multiply_route_states WHERE route_key=$1`, routeKey).Scan(&raw, &marker, &version); err != nil {
		return false, nil, err
	}
	if string(raw) == "null" {
		if string(marker) != "null" {
			return false, nil, budgetHold("pilot_marker_without_budget")
		}
		return false, nil, nil
	}
	var b Phase3Budget
	if json.Unmarshal(raw, &b) != nil {
		return false, nil, budgetHold("invalid_durable_budget")
	}
	if err := b.validate(); err != nil {
		return false, nil, err
	}
	if b.Pilot == nil {
		if string(marker) != "null" {
			return false, nil, budgetHold("pilot_marker_without_authority")
		}
		return false, nil, nil
	}
	activation, err := validatePersistedPilotActivation(b, marker, version)
	if err != nil {
		return false, nil, err
	}
	// A closed pilot is inactive: its archived baseline must not explain
	// anything on a later route, so no baseline leaves this read.
	if b.Closed {
		return false, nil, nil
	}
	// validatePersistedPilotActivation already revalidates the archived flat
	// evidence; decoding the ticket out of it cannot fail here without the
	// evidence itself being invalid. The ticket is disarmed and flat by that
	// validation, so only its consumed sequence is carried out. Squads
	// sequences are slot-ordered, so a consumed sequence beyond the
	// activation slot (or beyond int64) cannot belong to this baseline.
	ticket, err := decodeObservedReportTicket(accountAt(activation.FlatEvidence.Accounts, reportTicketPDA))
	if err != nil {
		return false, nil, err
	}
	if ticket.LastConsumedSequence > uint64(math.MaxInt64) || ticket.LastConsumedSequence > uint64(activation.FlatEvidence.Slot) {
		return false, nil, budgetHold("incoherent_pilot_activation_evidence")
	}
	return true, &pilotActivationBaseline{
		TicketLastConsumedSequenceRaw: int64(ticket.LastConsumedSequence),
		FinalizedSlot:                 activation.FlatEvidence.Slot,
	}, nil
}

func (d *Database) PilotRuntimeEnabled(ctx context.Context, routeKey string) (bool, error) {
	active, _, err := d.PilotRuntimeState(ctx, routeKey)
	return active, err
}

func workingTrancheCap(s Snapshot) int64 {
	if s.PilotActive && selectorLane(s.RouteLane) {
		return PilotWorkingTrancheCapRaw
	}
	return Phase3WorkingTrancheCapRaw
}
