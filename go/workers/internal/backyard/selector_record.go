package backyard

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// RecordSelectorEvaluation consumes a complete economic quote, never a shadow
// ranking, and stores the result: the rates' choice target() reads (a flat
// vault enters the lane of its latest ENTER). Nothing here writes a
// transaction or any spending authority, so an operation in flight or a
// manual stop does not refuse it: the tick reads the choice as an input.
func (d *Database) RecordSelectorEvaluation(ctx context.Context, routeKey string, input SelectorInput, confirmedSlot int64) (SelectorResult, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var result SelectorResult
	// The pure selector also serves diagnostic model snapshots. Only a
	// coherent, monitors-armed book may write the entry choice here,
	// including operator canaries.
	if !input.Snapshot.MonitorsArmed {
		return result, budgetHold("selector_fee_evidence_unavailable")
	}
	now := time.Now().UTC()
	if input.Now.After(now) || now.Sub(input.Now) > 5*time.Second || input.Snapshot.Slot <= 0 || confirmedSlot < input.Snapshot.Slot || confirmedSlot-input.Snapshot.Slot > observationLagSlots() {
		return result, budgetHold("selector_evaluation_not_current")
	}
	err := d.inTx(ctx, func(tx pgx.Tx) (err error) {
		var raw []byte
		if err = tx.QueryRow(ctx, `SELECT state FROM loyal_yield.multiply_route_states WHERE route_key=$1 FOR UPDATE`, routeKey).Scan(&raw); err != nil {
			return err
		}
		var state struct {
			CanaryHistory map[string]pilotCanaryEntryReceipt `json:"pilotCanaryEntries"`
			Selector      struct {
				Result SelectorResult `json:"result"`
			} `json:"selector"`
		}
		if json.Unmarshal(raw, &state) != nil {
			return budgetHold("invalid_selector_route_state")
		}
		now = time.Now().UTC()
		if now.Sub(input.Now) > 5*time.Second {
			return budgetHold("selector_evaluation_not_current")
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
			return err
		}
		encoded, err := json.Marshal(map[string]any{"mode": "live", "observationId": input.Snapshot.ObservationID, "slot": input.Snapshot.Slot, "result": result})
		if err != nil {
			return err
		}
		var updated map[string]json.RawMessage
		if err = json.Unmarshal(raw, &updated); err != nil {
			return err
		}
		updated["selector"] = encoded
		if canaryReceipt != nil {
			if state.CanaryHistory == nil {
				state.CanaryHistory = make(map[string]pilotCanaryEntryReceipt)
			}
			state.CanaryHistory[canaryReceipt.Request.ID] = *canaryReceipt
			updated["pilotCanaryEntries"], err = json.Marshal(state.CanaryHistory)
			if err != nil {
				return err
			}
		}
		stateJSON, err := json.Marshal(updated)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=$2::jsonb,updated_at=clock_timestamp() WHERE route_key=$1`, routeKey, string(stateJSON)); err != nil {
			return err
		}
		return nil
	})
	return result, err
}
