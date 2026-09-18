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
		e.Quote.MinimumIdleRaw < uint64(e.EquityRaw) || !e.Quote.validBorrow() || !e.Quote.currentAtSlot(e.Quote.SampleSlot) || e.AcceptedAt.IsZero() || e.Quote.ObservedAt.IsZero() || e.Quote.ObservedAt.After(e.AcceptedAt) || !e.ExpiresAt.After(e.AcceptedAt) || e.ExpiresAt.After(e.Quote.ObservedAt.Add(30*time.Second)) {
		return fmt.Errorf("invalid_selector_entry")
	}
	return nil
}

func hasWorkingCapital(s Snapshot) bool {
	return s.HasPosition || s.PositionCollateralRaw > 0 || s.PositionDebtRaw > 0 || s.CollateralIdleRaw > 0 || s.PrimeIdleRaw > 0 || s.SquadsIdleRaw > 0 || s.DebtIdleRaw > 0 || s.VoltrStrategyIdleRaw > 0
}

func applySelectorEntry(s *Snapshot, entry *SelectorEntry, now time.Time) error {
	s.SelectorEntryEquityRaw = 0
	s.SelectorBorrowRaw = 0
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
	if !hasWorkingCapital(*s) && (entry.AllocationOperationID != "" || !entry.Quote.currentAtSlot(s.Slot) || now.Before(entry.AcceptedAt) || !now.Before(entry.ExpiresAt)) {
		s.SelectorEntryPaused = true
		return nil
	}
	s.SelectorEntryEquityRaw = entry.EquityRaw
	s.SelectorBorrowRaw = entry.Quote.BorrowReceiveRaw
	return nil
}

func (d *Database) LoadSelectorEntry(ctx context.Context, routeKey string) (*SelectorEntry, error) {
	var raw []byte
	if err := d.pool.QueryRow(ctx, `SELECT state->'selectorEntry' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, routeKey).Scan(&raw); err != nil {
		return nil, err
	}
	return decodeSelectorEntry(raw)
}

func decodeSelectorEntry(raw []byte) (*SelectorEntry, error) {
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
// ENTER opens only a flat, reconciled lane. SWITCH commits the bounded source
// unwind in this same transaction, using only existing reserved exit spending.
// Neither action writes an executable transaction.
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
		Budget        Phase3Budget                       `json:"phase3"`
		Activation    json.RawMessage                    `json:"pilotBudgetActivation"`
		Unwind        *UnwindIntent                      `json:"selectorUnwind"`
		CanaryHistory map[string]pilotCanaryEntryReceipt `json:"pilotCanaryEntries"`
		Selector      struct {
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
	// The source observation may precede the final fee/price collection. Use
	// the caller's latest confirmed slot as well as that source slot; a fresh
	// timestamp alone cannot extend any component of the economic recipe.
	currentQuotes := make([]MoveQuote, 0, len(input.Quotes))
	for _, q := range input.Quotes {
		if q.currentAtSlot(confirmedSlot) {
			currentQuotes = append(currentQuotes, q)
		}
	}
	input.Quotes = currentQuotes
	result = SelectOpportunity(input, state.Selector.Result.State)
	result, canaryReceipt, err := selectPilotCanaryEntry(input, result, state.CanaryHistory)
	if err != nil {
		return result, err
	}
	var entry *SelectorEntry
	if result.Action == "ENTER" || result.Action == "CANARY_ENTER" {
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
		if canaryReceipt != nil && canaryReceipt.Request.ExpiresAt.Before(entry.ExpiresAt) {
			entry.ExpiresAt = canaryReceipt.Request.ExpiresAt
		}
		if err = entry.validate(); err != nil {
			return result, err
		}
		if entry.EquityRaw > s.VoltrIdleRaw {
			return result, budgetHold("selector_entry_cash_changed")
		}
	}
	var unwind *UnwindIntent
	if result.Action == "SWITCH" {
		q, s := result.SelectedQuote, input.Snapshot
		if q == nil || q.SourceExit == nil || q.SourceLane != s.RouteLane || q.ObservationID != s.ObservationID || !sha256Pattern.MatchString(q.EvidenceID) || q.SourceExit.MaxCollateralRaw != s.PositionCollateralRaw || q.SourceExit.MaxDebtRaw < s.PositionDebtRaw || s.PositionDebtRaw < 0 || s.PositionCollateralRaw < 0 {
			return result, budgetHold("selector_unwind_quote_missing")
		}
		unwind = &UnwindIntent{SourceLane: s.RouteLane, Reason: "economic_rotation", ObservationID: s.ObservationID, MaxCollateralRaw: q.SourceExit.MaxCollateralRaw, MaxDebtRaw: q.SourceExit.MaxDebtRaw, CostBoundRaw: q.SourceExit.GrossMicros, BudgetScope: state.Budget.GoalID, BudgetFamily: phase3BudgetFamilyForLane(s.RouteLane), EvidenceID: q.EvidenceID, CreatedAt: now}
		if err = unwind.validate(); err != nil {
			return result, err
		}
		if state.Budget.Families[unwind.BudgetFamily].ExitMicros < unwind.CostBoundRaw {
			return result, budgetHold("unwind_requires_existing_exit_reservation")
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
	// Change the selected authority and its result under one route fence. A
	// switch clears entry permission; destination selection starts afresh only
	// after the source has reconciled flat.
	var updated map[string]json.RawMessage
	if err = json.Unmarshal(raw, &updated); err != nil {
		return result, err
	}
	updated["selector"] = encoded
	if canaryReceipt != nil {
		if state.CanaryHistory == nil {
			state.CanaryHistory = make(map[string]pilotCanaryEntryReceipt)
		}
		state.CanaryHistory[canaryReceipt.Request.ID] = *canaryReceipt
		updated["pilotCanaryEntries"], err = json.Marshal(state.CanaryHistory)
		if err != nil {
			return result, err
		}
	}
	nextVersion := version
	if entry != nil || unwind != nil {
		nextVersion++
		updated["generation"], _ = json.Marshal(nextVersion)
		if entry != nil {
			updated["selectorEntry"] = entryJSON
			updated["selectorEntryPaused"] = json.RawMessage("false")
		} else {
			updated["selectorUnwind"], err = json.Marshal(unwind)
			if err != nil {
				return result, err
			}
			updated["selectorEntry"] = json.RawMessage("null")
			updated["selectorEntryPaused"] = json.RawMessage("true")
		}
	}
	stateJSON, err := json.Marshal(updated)
	if err != nil {
		return result, err
	}
	tag, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=$5::jsonb,state_version=$6,updated_at=clock_timestamp() WHERE route_key=$1 AND lease_owner=$2 AND fencing_token=$3 AND state_version=$4 AND lease_expires_at>clock_timestamp()`, routeKey, lease.Owner, lease.FencingToken, version, string(stateJSON), nextVersion)
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
func (d *Database) authorizeSelectorEntryTx(ctx context.Context, tx pgx.Tx, operationID string, budget Phase3Budget, request any, effects ExpectedEffects, slot int64, admission bool) error {
	if budget.Pilot == nil {
		return nil
	}
	var amount uint64
	var requestedLane string
	borrow := false
	switch r := request.(type) {
	case BridgeBuildRequest:
		if r.Action != VoltrAllocateToSquads {
			return nil
		}
		amount = r.AmountRaw
	case KaminoInitializationRequest:
		requestedLane = r.RouteLane
	case KaminoPrimeUSDCRequest:
		_, leg, err := kaminoPrimeUSDCInstruction(r)
		if err != nil {
			return err
		}
		if leg != kaminoLegBorrow {
			return nil
		}
		requestedLane, amount, borrow = r.RouteLane, r.AmountRaw, true
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
	if borrow {
		if entry.AllocationOperationID == "" || amount != entry.Quote.BorrowReceiveRaw {
			return budgetHold("selector_entry_borrow_mismatch")
		}
		debit, err := MeasureExecutableDebit(request, effects)
		if err != nil {
			return err
		}
		if debit.Raw < amount || debit.Raw-amount > entry.Quote.BorrowFeeRaw {
			return budgetHold("selector_entry_borrow_fee_exceeded")
		}
		// The tranche is funded. Time/slot expiry closes new allocation, not
		// completion; current position, risk and costs are still checked per leg.
		return nil
	}
	now := time.Now().UTC()
	if !entry.Quote.currentAtSlot(slot) || now.Before(entry.AcceptedAt) || !now.Before(entry.ExpiresAt) {
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

// Hold when the reviewed amount is no longer supportable. Do not silently
// change the swap input or origination fee used by the complete move forecast.
func selectorBorrowAmount(s Snapshot, currentTarget uint64) (uint64, error) {
	if !s.PilotActive {
		return currentTarget, nil
	}
	if s.SelectorBorrowRaw == 0 || s.SelectorBorrowRaw > currentTarget || s.SelectorEntryPaused || s.Unwind || s.CutoverDrain || s.WithdrawalDemandRaw > 0 {
		return 0, budgetHold("selector_entry_borrow_unavailable")
	}
	return s.SelectorBorrowRaw, nil
}
