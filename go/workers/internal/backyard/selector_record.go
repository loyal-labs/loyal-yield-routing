package backyard

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// RecordSelectorEvaluation consumes a complete economic quote, never a shadow
// ranking. It re-evaluates persistence under the same route lock as execution
// and stores the result, the rates' choice target() reads: a flat vault
// enters the lane of its latest ENTER. expectedVersion must be read before
// collecting the account observation and quotes; completing an intervening
// operation invalidates the entire sample. Nothing here writes a transaction
// or any spending authority.
func (d *Database) RecordSelectorEvaluation(ctx context.Context, routeKey string, input SelectorInput, confirmedSlot, expectedVersion int64) (SelectorResult, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var result SelectorResult
	// The pure selector also serves diagnostic model snapshots. Only a
	// coherent, monitors-armed book may write entry/unwind authority here.
	// Reject before the lease/DB boundary, including operator canaries.
	if !input.Snapshot.MonitorsArmed {
		return result, budgetHold("selector_fee_evidence_unavailable")
	}
	now := time.Now().UTC()
	if input.Now.After(now) || now.Sub(input.Now) > 5*time.Second || input.Snapshot.Slot <= 0 || confirmedSlot < input.Snapshot.Slot || confirmedSlot-input.Snapshot.Slot > observationLagSlots() {
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
		CanaryHistory map[string]pilotCanaryEntryReceipt `json:"pilotCanaryEntries"`
		Selector      struct {
			Result SelectorResult `json:"result"`
		} `json:"selector"`
	}
	if json.Unmarshal(raw, &state) != nil {
		return result, budgetHold("invalid_selector_route_state")
	}
	// The two operation-table guard checks ride ONE SELECT whose columns wrap
	// the unchanged guard predicates (order, hold codes and precedence
	// unchanged; one sequential statement round trip saved). The manual latch
	// read stays a separate statement AFTER them — LatchManualRecovery writes
	// the latch without this route row lock, so its snapshot must not move earlier. Proof: docs/plans/voltr-auto-expansion/51-agent-b-admission-result.md.
	var opsPending, recoveryPending bool
	if err = tx.QueryRow(ctx, `SELECT `+
		`EXISTS(SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key=$1 AND status IN (`+nonterminalStatusSQL+`)), `+
		`(`+UnresolvedCapitalRecoverySQL+`)`, routeKey).Scan(&opsPending, &recoveryPending); err != nil {
		return result, err
	}
	if opsPending {
		return result, budgetHold("selector_finish_current_work_first")
	}
	if recoveryPending {
		return result, budgetHold("selector_resolve_capital_recovery_first")
	}
	var manualPending bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key=$1 AND cleared_at IS NULL) OR EXISTS (`+manualRecoveryDerivedLatchSQL+`)`, routeKey).Scan(&manualPending); err != nil {
		return result, err
	}
	if manualPending {
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
	var canaryReceipt *pilotCanaryEntryReceipt
	result, canaryReceipt, err = selectPilotCanaryEntry(input, result, state.CanaryHistory)
	if err != nil {
		return result, err
	}
	encoded, err := json.Marshal(map[string]any{"mode": "live", "observationId": input.Snapshot.ObservationID, "slot": input.Snapshot.Slot, "result": result})
	if err != nil {
		return result, err
	}
	// The result is the rates' choice and moves no money. A change of the
	// lane a flat vault enters is a new target input, so it advances the
	// generation and a tick planned on the old choice re-reads; a fresh
	// quote for the same choice does not.
	bump := int64(0)
	if entryChoice(result) != entryChoice(state.Selector.Result) {
		bump = 1
	}
	var updated map[string]json.RawMessage
	if err = json.Unmarshal(raw, &updated); err != nil {
		return result, err
	}
	updated["selector"] = encoded
	if bump > 0 {
		updated["generation"], _ = json.Marshal(version + bump)
	}
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
	stateJSON, err := json.Marshal(updated)
	if err != nil {
		return result, err
	}
	tag, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=$5::jsonb,state_version=state_version+$6,updated_at=clock_timestamp() WHERE route_key=$1 AND lease_owner=$2 AND fencing_token=$3 AND state_version=$4 AND lease_expires_at>clock_timestamp()`, routeKey, lease.Owner, lease.FencingToken, version, string(stateJSON), bump)
	if err != nil {
		return result, err
	}
	if tag.RowsAffected() != 1 {
		return result, fmt.Errorf("selector_write_lost_route_fence")
	}
	return result, tx.Commit(ctx)
}

// entryChoice is the lane a selector result enters, "" for none.
func entryChoice(r SelectorResult) string {
	if r.Action != "ENTER" && r.Action != "CANARY_ENTER" {
		return ""
	}
	return r.DestinationLane
}
