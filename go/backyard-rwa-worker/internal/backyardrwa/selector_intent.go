package backyardrwa

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// UnwindIntent is bounded work, not a second ledger. BudgetScope/Family point
// to the existing durable exit reservation. Current balances determine each
// next step; the destination is deliberately not promised during an unwind.
type UnwindIntent struct {
	SourceLane       string    `json:"sourceLane"`
	Reason           string    `json:"reason"`
	ObservationID    string    `json:"observationId"`
	MaxCollateralRaw int64     `json:"maxCollateralRaw"`
	MaxDebtRaw       int64     `json:"maxDebtRaw"`
	CostBoundRaw     int64     `json:"costBoundRaw"`
	BudgetScope      string    `json:"budgetScope"`
	BudgetFamily     string    `json:"budgetFamily"`
	EvidenceID       string    `json:"evidenceId"`
	CreatedAt        time.Time `json:"createdAt"`
}

func (i UnwindIntent) validate() error {
	if !selectorLane(i.SourceLane) || (i.Reason != "economic_rotation" && i.Reason != "withdrawal_shortfall") || i.ObservationID == "" || i.MaxCollateralRaw < 0 || i.MaxDebtRaw < 0 || i.CostBoundRaw <= 0 || i.BudgetScope == "" || i.BudgetFamily == "" || i.BudgetFamily != phase3BudgetFamilyForLane(i.SourceLane) || !sha256Pattern.MatchString(i.EvidenceID) || i.CreatedAt.IsZero() {
		return fmt.Errorf("invalid_unwind_intent")
	}
	return nil
}
func applyUnwindIntent(s *Snapshot, intent *UnwindIntent) error {
	if intent == nil {
		return nil
	}
	if err := intent.validate(); err != nil {
		return err
	}
	if s.RouteLane != intent.SourceLane {
		return fmt.Errorf("unwind_source_changed_before_reconciliation")
	}
	// The admitted debt bound includes the payoff interest window. Exceeding it
	// requires fresh admission, not a silent increase in the committed envelope.
	if s.PositionCollateralRaw > intent.MaxCollateralRaw || s.PositionDebtRaw > intent.MaxDebtRaw {
		return fmt.Errorf("unwind_holdings_exceed_admitted_bounds")
	}
	s.Unwind = true
	return nil
}
func unwindComplete(s Snapshot) bool {
	return s.Fresh && s.ManualReason == "" && s.Nonterminal == "" && !s.HasAmbiguousSubmission && !s.HasPosition && s.PositionDebtRaw == 0 && s.PositionCollateralRaw == 0 && s.CollateralIdleRaw == 0 && s.PrimeIdleRaw == 0 && s.SquadsIdleRaw == 0 && s.DebtIdleRaw == 0 && s.VoltrStrategyIdleRaw == 0 && s.StrategyNAVRaw == 0 && s.PriorReportedNAVRaw == 0 && !s.PostMutationNAVRequired && !s.CapitalMutated
}
func (d *Database) LoadUnwindIntent(ctx context.Context, routeKey string) (*UnwindIntent, error) {
	var raw []byte
	if err := d.pool.QueryRow(ctx, `SELECT state->'selectorUnwind' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, routeKey).Scan(&raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var intent UnwindIntent
	if err := json.Unmarshal(raw, &intent); err != nil {
		return nil, fmt.Errorf("invalid_durable_unwind")
	}
	if err := intent.validate(); err != nil {
		return nil, err
	}
	return &intent, nil
}

// CommitUnwindIntent can only refer to an already funded exit in the existing
// budget. It never creates spending room or loosens a closed campaign. The same
// route lease/lock and no-nonterminal fence serialize it with RecordDecision.
func (d *Database) CommitUnwindIntent(ctx context.Context, routeKey string, intent UnwindIntent) error {
	if err := intent.validate(); err != nil {
		return err
	}
	lease, err := d.currentLease()
	if err != nil {
		return err
	}
	if lease.RouteKey != routeKey {
		return fmt.Errorf("unwind_route_lease_mismatch")
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var version int64
	var raw []byte
	if err = tx.QueryRow(ctx, RouteStateForUpdate, routeKey, lease.Owner, lease.FencingToken).Scan(&version, &raw); err != nil {
		return err
	}
	var state struct {
		Budget Phase3Budget  `json:"phase3"`
		Unwind *UnwindIntent `json:"selectorUnwind"`
	}
	if err = json.Unmarshal(raw, &state); err != nil {
		return err
	}
	if state.Unwind != nil {
		if !sameUnwindIntent(*state.Unwind, intent) {
			return budgetHold("another_unwind_is_committed")
		}
		return tx.Commit(ctx)
	}
	if err = state.Budget.validate(); err != nil {
		return err
	}
	if state.Budget.GoalID != intent.BudgetScope || state.Budget.Closed || len(state.Budget.Reservations) != 0 || state.Budget.Families[intent.BudgetFamily].ExitMicros < intent.CostBoundRaw {
		return budgetHold("unwind_requires_existing_exit_reservation")
	}
	var blocked bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key=$1 AND status IN ('decided','built','simulated','signed','broadcast_intent','submitted','confirmed','reconciling'))`, routeKey).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return budgetHold("recover_transaction_before_committing_unwind")
	}
	if err = tx.QueryRow(ctx, UnresolvedCapitalRecoverySQL, routeKey).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return budgetHold("resolve_capital_recovery_before_unwind")
	}
	encoded, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	return d.writeUnwindTx(ctx, tx, routeKey, version, encoded)
}
func (d *Database) writeUnwindTx(ctx context.Context, tx pgx.Tx, routeKey string, version int64, encoded []byte) error {
	lease, err := d.currentLease()
	if err != nil {
		return err
	}
	if lease.RouteKey != routeKey {
		return fmt.Errorf("unwind_route_lease_mismatch")
	}
	tag, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(jsonb_set(CASE WHEN $5::jsonb='null'::jsonb THEN jsonb_set(state,'{selectorEntryPaused}','true'::jsonb,true) ELSE state END,'{selectorUnwind}',$5::jsonb,true),'{generation}',to_jsonb(state_version+1),true),state_version=state_version+1,updated_at=clock_timestamp() WHERE route_key=$1 AND lease_owner=$2 AND fencing_token=$3 AND state_version=$4 AND lease_expires_at>clock_timestamp()`, routeKey, lease.Owner, lease.FencingToken, version, string(encoded))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("unwind_write_lost_route_fence")
	}
	return tx.Commit(ctx)
}

// Completion requires independently observed flat holdings and current NAV;
// an intent never clears simply because its latest transaction succeeded.
func (d *Database) CompleteUnwindIntent(ctx context.Context, routeKey string, intent UnwindIntent, s Snapshot) error {
	if err := intent.validate(); err != nil {
		return err
	}
	if !unwindComplete(s) || s.RouteLane != intent.SourceLane {
		return fmt.Errorf("unwind_not_reconciled_flat")
	}
	lease, err := d.currentLease()
	if err != nil {
		return err
	}
	if lease.RouteKey != routeKey {
		return fmt.Errorf("unwind_route_lease_mismatch")
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var version int64
	var raw []byte
	if err = tx.QueryRow(ctx, RouteStateForUpdate, routeKey, lease.Owner, lease.FencingToken).Scan(&version, &raw); err != nil {
		return err
	}
	var state struct {
		Unwind *UnwindIntent `json:"selectorUnwind"`
	}
	if json.Unmarshal(raw, &state) != nil || state.Unwind == nil || !sameUnwindIntent(*state.Unwind, intent) {
		return fmt.Errorf("unwind_identity_changed")
	}
	var pending bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key=$1 AND status IN (`+nonterminalStatusSQL+`))`, routeKey).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("unwind_completion_has_pending_transaction")
	}
	return d.writeUnwindTx(ctx, tx, routeKey, version, []byte("null"))
}

func (d *Database) SelectorEntryPaused(ctx context.Context, routeKey string) (bool, error) {
	var paused bool
	err := d.pool.QueryRow(ctx, `SELECT COALESCE((state->>'selectorEntryPaused')::boolean,false) FROM loyal_yield.multiply_route_states WHERE route_key=$1`, routeKey).Scan(&paused)
	return paused, err
}

func sameUnwindIntent(a, b UnwindIntent) bool {
	sameTime := a.CreatedAt.Equal(b.CreatedAt)
	a.CreatedAt = b.CreatedAt
	return sameTime && a == b
}
