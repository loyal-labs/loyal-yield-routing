package backyard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"time"

	"github.com/jackc/pgx/v5"
)

// PilotExecutionCostRecompute replaces the execution-cost bound booked by each
// finalized pilot operation with its realized cost, where the operation's
// persisted send cost and finalized effects prove it. An unproven operation
// keeps its booked bound. Failed-report fees were booked from their measured
// receipt and are untouched. Nothing is reset or deleted.
type PilotExecutionCostRecompute struct {
	Schema     string                                       `json:"schema"`
	RouteKey   string                                       `json:"routeKey"`
	CapMicros  int64                                        `json:"capMicros"`
	Executed   bool                                         `json:"executed"`
	Operations []PilotExecutionCostRecomputeOperation       `json:"operations"`
	Families   map[string]PilotExecutionCostRecomputeFamily `json:"families"`
}

type PilotExecutionCostRecomputeOperation struct {
	OperationID    string `json:"operationId"`
	Family         string `json:"family"`
	BookedMicros   int64  `json:"bookedMicros"`
	RealizedMicros int64  `json:"realizedMicros,omitempty"`
	Unproven       string `json:"unproven,omitempty"`
}

type PilotExecutionCostRecomputeFamily struct {
	BeforeMicros int64 `json:"beforeMicros"`
	AfterMicros  int64 `json:"afterMicros"`
}

// RunPilotExecutionCostRecompute is explicit operator bookkeeping: no signer,
// RPC or chain write. A dry run reads one consistent snapshot; execute takes a
// short route lease so its write is fenced like every other budget write.
func RunPilotExecutionCostRecompute(ctx context.Context, databaseURL, routeKey string, execute bool) (result PilotExecutionCostRecompute, err error) {
	if routeKey != productionRouteKey || databaseURL == "" {
		return result, budgetHold("invalid_pilot_cost_recompute_config")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	db, err := OpenDatabase(ctx, databaseURL)
	if err != nil {
		return result, budgetHold("pilot_cost_recompute_database_unavailable")
	}
	defer db.Close()
	if execute {
		var nonce [16]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			return result, budgetHold("pilot_cost_recompute_owner_unavailable")
		}
		if _, err = db.AcquireRouteLease(ctx, routeKey, "pilot-cost-recompute:"+hex.EncodeToString(nonce[:]), 45*time.Second); err != nil {
			return result, budgetHold("pilot_cost_recompute_lease_unavailable")
		}
		defer func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			released, releaseErr := db.ReleaseRouteLease(releaseCtx)
			if err == nil && (releaseErr != nil || !released) {
				err = budgetHold("pilot_cost_recompute_lease_release_unconfirmed")
			}
		}()
	}
	result, err = db.recomputePilotExecutionCost(ctx, routeKey, execute)
	if err != nil {
		var hold *BudgetHold
		if errors.As(err, &hold) {
			return result, hold
		}
		// SQL failures may embed credentials. Emit only a fixed stage error.
		return result, budgetHold("pilot_cost_recompute_not_confirmed")
	}
	return result, nil
}

// recomputePilotExecutionCost works in one short transaction. With execute it
// locks the route row under this process's lease, refuses any open reservation
// or nonterminal operation, and writes the family totals and every changed
// operation's booked cost atomically.
func (d *Database) recomputePilotExecutionCost(ctx context.Context, routeKey string, execute bool) (PilotExecutionCostRecompute, error) {
	report := PilotExecutionCostRecompute{Schema: "backyard-pilot-execution-cost-recompute/v1", RouteKey: routeKey, CapMicros: PilotEntryExecutionCostCapMicros, Operations: []PilotExecutionCostRecomputeOperation{}, Families: map[string]PilotExecutionCostRecomputeFamily{}}
	options := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	if execute {
		options = pgx.TxOptions{}
	}
	tx, err := d.pool.BeginTx(ctx, options)
	if err != nil {
		return report, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var version int64
	var raw []byte
	if execute {
		lease, leaseErr := d.currentLease()
		if leaseErr != nil || lease.RouteKey != routeKey {
			return report, ErrRouteLeaseLost
		}
		err = tx.QueryRow(ctx, RouteStateForUpdate, lease.RouteKey, lease.Owner, lease.FencingToken).Scan(&version, &raw)
	} else {
		err = tx.QueryRow(ctx, `SELECT state_version,state FROM loyal_yield.multiply_route_states WHERE route_key=$1`, routeKey).Scan(&version, &raw)
	}
	if err != nil {
		return report, err
	}
	var state map[string]json.RawMessage
	var budget Phase3Budget
	if json.Unmarshal(raw, &state) != nil || json.Unmarshal(state["phase3"], &budget) != nil || budget.validate() != nil || budget.Pilot == nil {
		return report, budgetHold("recompute_requires_active_pilot_budget")
	}
	if _, err = validatePersistedPilotActivation(budget, state["pilotBudgetActivation"], version); err != nil {
		return report, err
	}
	if execute {
		var active bool
		if err = tx.QueryRow(ctx, manualRestoreNoNonterminalSQL, routeKey).Scan(&active); err != nil {
			return report, err
		}
		if active || len(budget.Reservations) != 0 {
			return report, budgetHold("recompute_requires_settled_route")
		}
	}
	rows, err := tx.Query(ctx, `SELECT operation_id,COALESCE(strategy_key,''),expected_effects->'phase3',reconciled_effects FROM loyal_yield.multiply_operations
		WHERE route_key=$1 AND status='reconciled' AND confirmation_status='finalized' AND reconciled_effects IS NOT NULL AND expected_effects->'phase3'->>'pilotAuthorityId'=$2
		ORDER BY confirmed_slot,operation_id`, routeKey, budget.Pilot.AuthorityID)
	if err != nil {
		return report, err
	}
	defer rows.Close()
	families := maps.Clone(budget.Families)
	changed := map[string]int64{}
	for rows.Next() {
		var id, lane string
		var authBytes, reconciled []byte
		if err = rows.Scan(&id, &lane, &authBytes, &reconciled); err != nil {
			return report, err
		}
		var auth phase3OperationAuthorization
		op := PilotExecutionCostRecomputeOperation{OperationID: id, Family: phase3BudgetFamilyForLane(lane)}
		row, known := families[op.Family]
		if json.Unmarshal(authBytes, &auth) != nil || !known || auth.GoalID != Phase3GoalID || auth.ReservationReleased || auth.BookedExecutionCostMicros <= 0 {
			op.Unproven = "unbooked_or_unbound_operation"
			report.Operations = append(report.Operations, op)
			continue
		}
		op.BookedMicros = auth.BookedExecutionCostMicros
		realized, realizedErr := realizedPilotExecutionCost(auth, reconciled)
		var hold *BudgetHold
		switch {
		case errors.As(realizedErr, &hold):
			op.Unproven = hold.Reason
		case realizedErr != nil:
			return report, realizedErr
		case realized > op.BookedMicros:
			op.Unproven = "realized_exceeds_booked_bound"
		default:
			op.RealizedMicros = realized
			row.ExecutionCostSpentMicros -= op.BookedMicros - realized
			families[op.Family] = row
			if realized != op.BookedMicros {
				changed[id] = realized
			}
		}
		report.Operations = append(report.Operations, op)
	}
	if err = rows.Err(); err != nil {
		return report, err
	}
	next := budget
	next.Families = families
	if next.validate() != nil {
		return report, budgetHold("recompute_invalid_family_totals")
	}
	for family, row := range budget.Families {
		report.Families[family] = PilotExecutionCostRecomputeFamily{row.ExecutionCostSpentMicros, families[family].ExecutionCostSpentMicros}
	}
	if !execute {
		return report, nil
	}
	if err = d.writePhase3RouteBudgetTx(ctx, tx, next); err != nil {
		return report, err
	}
	for id, realized := range changed {
		result, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=jsonb_set(expected_effects,'{phase3,bookedExecutionCostMicros}',to_jsonb($2::bigint),true),updated_at=clock_timestamp() WHERE operation_id=$1 AND status='reconciled'`, id, realized)
		if err != nil {
			return report, err
		}
		if result.RowsAffected() != 1 {
			return report, budgetHold("recompute_operation_changed")
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return report, err
	}
	report.Executed = true
	return report, nil
}
