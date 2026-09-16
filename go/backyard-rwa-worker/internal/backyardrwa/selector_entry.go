package backyardrwa

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// A selected entry pins the next lane and the exact economically quoted equity.
// It owns no balance or spending counter. Every transaction still requires the
// existing durable budget admission, fresh construction and reconciliation.
type SelectorEntry struct {
	Lane          string    `json:"lane"`
	EquityRaw     int64     `json:"equityRaw"`
	ObservationID string    `json:"observationId"`
	Quote         MoveQuote `json:"quote"`
	AcceptedAt    time.Time `json:"acceptedAt"`
	ExpiresAt     time.Time `json:"expiresAt"`
	// A choice funds one allocation attempt. Retries keep the same operation;
	// a returned/expired attempt needs a fresh observation and quote.
	AllocationOperationID string `json:"allocationOperationId,omitempty"`
}

func (e SelectorEntry) validate() error {
	if !selectorLane(e.Lane) || e.ObservationID == "" || e.EquityRaw <= 0 || e.EquityRaw > PilotWorkingTrancheCapRaw ||
		e.Quote.DestinationLane != e.Lane || !selectorLane(e.Quote.SourceLane) || e.Quote.ObservationID != e.ObservationID || e.Quote.EquityRaw != e.EquityRaw || e.Quote.CostRaw < 0 || e.Quote.CostRaw >= e.EquityRaw || !sha256Pattern.MatchString(e.Quote.EvidenceID) ||
		e.AcceptedAt.IsZero() || e.Quote.ObservedAt.IsZero() || e.Quote.ObservedAt.After(e.AcceptedAt) || !e.ExpiresAt.After(e.AcceptedAt) || e.ExpiresAt.After(e.Quote.ObservedAt.Add(30*time.Second)) {
		return fmt.Errorf("invalid_selector_entry")
	}
	return nil
}

func hasWorkingCapital(s Snapshot) bool {
	return s.HasPosition || s.PositionCollateralRaw > 0 || s.PositionDebtRaw > 0 || s.CollateralIdleRaw > 0 || s.PrimeIdleRaw > 0 || s.SquadsIdleRaw > 0 || s.DebtIdleRaw > 0 || s.VoltrStrategyIdleRaw > 0
}

func applySelectorEntry(s *Snapshot, entry *SelectorEntry, now time.Time) error {
	s.SelectorEntryEquityRaw = 0
	if !s.PilotActive {
		return nil
	}
	if entry == nil {
		s.SelectorEntryPaused = true
		return nil
	}
	if err := entry.validate(); err != nil {
		return err
	}
	if entry.Lane != s.RouteLane {
		s.SelectorEntryPaused = true
		return nil
	}
	// Expiry stops a new allocation, never interrupts the already allocated
	// tranche. Risk, withdrawals and return-to-idle retain their earlier priority.
	if !hasWorkingCapital(*s) && (entry.AllocationOperationID != "" || now.Before(entry.AcceptedAt) || !now.Before(entry.ExpiresAt)) {
		s.SelectorEntryPaused = true
		return nil
	}
	s.SelectorEntryEquityRaw = entry.EquityRaw
	return nil
}

func (d *Database) LoadSelectorEntry(ctx context.Context, routeKey string) (*SelectorEntry, error) {
	var raw []byte
	if err := d.pool.QueryRow(ctx, `SELECT state->'selectorEntry' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, routeKey).Scan(&raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var entry SelectorEntry
	if json.Unmarshal(raw, &entry) != nil {
		return nil, fmt.Errorf("invalid_durable_selector_entry")
	}
	if err := entry.validate(); err != nil {
		return nil, err
	}
	return &entry, nil
}

// RecordSelectorEvaluation consumes a complete economic quote, never a shadow
// ranking. It re-evaluates persistence under the same route lock as execution.
// expectedVersion must be read before collecting the account observation and
// quotes; completing an intervening operation invalidates the entire sample.
// ENTER opens only a flat, reconciled lane; SWITCH remains a result for the
// separately bounded existing unwind commitment. This writes no transaction.
func (d *Database) RecordSelectorEvaluation(ctx context.Context, routeKey string, input SelectorInput, confirmedSlot, expectedVersion int64) (SelectorResult, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var result SelectorResult
	now := time.Now().UTC()
	if !input.Snapshot.PilotActive || input.Now.After(now) || now.Sub(input.Now) > 5*time.Second || input.Snapshot.Slot <= 0 || confirmedSlot < input.Snapshot.Slot || confirmedSlot-input.Snapshot.Slot > budgetMaxObservationLagSlots {
		return result, budgetHold("selector_evaluation_not_current")
	}
	// Recompute wall-clock quote expiry after waiting for the route lock below.
	lease, err := d.currentLease()
	if err != nil {
		return result, err
	}
	if lease.RouteKey != routeKey {
		return result, fmt.Errorf("selector_route_lease_mismatch")
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout = '25ms'`); err != nil {
		return result, err
	}
	var version int64
	var raw []byte
	if err = tx.QueryRow(ctx, RouteStateForUpdate, routeKey, lease.Owner, lease.FencingToken).Scan(&version, &raw); err != nil {
		return result, err
	}
	if version != expectedVersion {
		return result, budgetHold("selector_state_changed_during_quote")
	}
	var state struct {
		Budget     Phase3Budget    `json:"phase3"`
		Activation json.RawMessage `json:"pilotBudgetActivation"`
		Unwind     *UnwindIntent   `json:"selectorUnwind"`
		Selector   struct {
			Result SelectorResult `json:"result"`
		} `json:"selector"`
	}
	if json.Unmarshal(raw, &state) != nil {
		return result, budgetHold("invalid_selector_route_state")
	}
	if err = state.Budget.validate(); err != nil {
		return result, err
	}
	if state.Budget.Pilot == nil || state.Budget.Closed {
		return result, budgetHold("selector_requires_active_pilot")
	}
	if _, err = validatePersistedPilotActivation(state.Budget, state.Activation, version); err != nil {
		return result, err
	}
	if state.Unwind != nil || len(state.Budget.Reservations) != 0 {
		return result, budgetHold("selector_finish_current_work_first")
	}
	var pending bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key=$1 AND status IN (`+nonterminalStatusSQL+`))`, routeKey).Scan(&pending); err != nil {
		return result, err
	}
	if pending {
		return result, budgetHold("selector_finish_current_work_first")
	}
	if err = tx.QueryRow(ctx, UnresolvedCapitalRecoverySQL, routeKey).Scan(&pending); err != nil {
		return result, err
	}
	if pending {
		return result, budgetHold("selector_resolve_capital_recovery_first")
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key=$1 AND cleared_at IS NULL) OR EXISTS (`+manualRecoveryDerivedLatchSQL+`)`, routeKey).Scan(&pending); err != nil {
		return result, err
	}
	if pending {
		return result, budgetHold("selector_manual_recovery_active")
	}
	now = time.Now().UTC()
	if now.Sub(input.Now) > 5*time.Second {
		return result, budgetHold("selector_evaluation_not_current")
	}
	input.Now = now
	result = SelectOpportunity(input, state.Selector.Result.State)
	var entry *SelectorEntry
	if result.Action == "ENTER" {
		s := input.Snapshot
		if !unwindComplete(s) || s.WithdrawalDemandRaw != 0 || s.Unwind || s.CutoverDrain || s.VoltrIdleRaw <= 0 {
			return result, budgetHold("selector_entry_requires_reconciled_idle")
		}
		for _, family := range state.Budget.Families {
			if family.ExitMicros != 0 {
				return result, budgetHold("selector_entry_has_outstanding_exit")
			}
		}
		q := result.SelectedQuote
		if q == nil {
			return result, budgetHold("selector_entry_quote_missing")
		}
		entry = &SelectorEntry{Lane: result.DestinationLane, EquityRaw: q.EquityRaw, ObservationID: s.ObservationID, Quote: *q, AcceptedAt: now, ExpiresAt: q.ObservedAt.Add(min(input.Policy.QuoteMaxAge, 30*time.Second))}
		if err = entry.validate(); err != nil {
			return result, err
		}
		if entry.EquityRaw > s.VoltrIdleRaw {
			return result, budgetHold("selector_entry_cash_changed")
		}
	}
	encoded, err := json.Marshal(map[string]any{"mode": "live", "observationId": input.Snapshot.ObservationID, "slot": input.Snapshot.Slot, "result": result})
	if err != nil {
		return result, err
	}
	entryJSON, err := json.Marshal(entry)
	if err != nil {
		return result, err
	}
	tag, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(CASE WHEN $6::jsonb='null'::jsonb THEN state ELSE jsonb_set(jsonb_set(jsonb_set(state,'{selectorEntry}',$6::jsonb,true),'{selectorEntryPaused}','false'::jsonb,true),'{generation}',to_jsonb(state_version+1),true) END,'{selector}',$5::jsonb,true),state_version=state_version+CASE WHEN $6::jsonb='null'::jsonb THEN 0 ELSE 1 END,updated_at=clock_timestamp() WHERE route_key=$1 AND lease_owner=$2 AND fencing_token=$3 AND state_version=$4 AND lease_expires_at>clock_timestamp()`, routeKey, lease.Owner, lease.FencingToken, version, string(encoded), string(entryJSON))
	if err != nil {
		return result, err
	}
	if tag.RowsAffected() != 1 {
		return result, fmt.Errorf("selector_write_lost_route_fence")
	}
	return result, tx.Commit(ctx)
}

// Called under the existing operation/route lock at admission, build and send.
// Expiry only closes a new allocation or account setup; completion and exits
// never depend on a still-current economic forecast.
func (d *Database) authorizeSelectorEntryTx(ctx context.Context, tx pgx.Tx, operationID string, budget Phase3Budget, request any, admission bool) error {
	if budget.Pilot == nil {
		return nil
	}
	var amount uint64
	var requestedLane string
	switch r := request.(type) {
	case BridgeBuildRequest:
		if r.Action != VoltrAllocateToSquads {
			return nil
		}
		amount = r.AmountRaw
	case KaminoInitializationRequest:
		requestedLane = r.RouteLane
	default:
		return nil
	}
	var raw []byte
	var paused, unwinding bool
	var lane string
	if err := tx.QueryRow(ctx, `SELECT s.state->'selectorEntry',COALESCE((s.state->>'selectorEntryPaused')::boolean,false),COALESCE(s.state->'selectorUnwind','null'::jsonb) <> 'null'::jsonb,COALESCE(o.strategy_key,'') FROM loyal_yield.multiply_route_states s JOIN loyal_yield.multiply_operations o USING(route_key) WHERE o.operation_id=$1`, operationID).Scan(&raw, &paused, &unwinding, &lane); err != nil {
		return err
	}
	var entry SelectorEntry
	if len(raw) == 0 || json.Unmarshal(raw, &entry) != nil || entry.validate() != nil || paused || unwinding || lane != entry.Lane || (requestedLane != "" && requestedLane != lane) || (requestedLane == "" && amount != uint64(entry.EquityRaw)) {
		return budgetHold("selector_entry_authority_mismatch")
	}
	now := time.Now().UTC()
	if now.Before(entry.AcceptedAt) || !now.Before(entry.ExpiresAt) {
		return budgetHold("selector_entry_quote_expired")
	}
	if requestedLane != "" {
		if entry.AllocationOperationID != "" {
			return budgetHold("selector_entry_already_allocated")
		}
		return nil
	}
	if entry.AllocationOperationID != "" && entry.AllocationOperationID != operationID {
		return budgetHold("selector_entry_already_allocated")
	}
	if !admission && entry.AllocationOperationID != operationID {
		return budgetHold("selector_entry_allocation_not_bound")
	}
	if admission && entry.AllocationOperationID == "" {
		// The caller holds the route row lock. This association commits with
		// measured admission or rolls back with it; it has no spending counter.
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states s SET state=jsonb_set(state,'{selectorEntry,allocationOperationId}',to_jsonb($1::text),true) FROM loyal_yield.multiply_operations o WHERE o.operation_id=$1 AND s.route_key=o.route_key`, operationID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return budgetHold("selector_entry_allocation_bind_failed")
		}
	}
	return nil
}
