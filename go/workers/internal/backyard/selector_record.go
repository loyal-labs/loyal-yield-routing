package backyard

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

func (d *Database) RecordSelectorEvaluation(ctx context.Context, routeKey string, input SelectorInput, confirmedSlot int64) (SelectorResult, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var result SelectorResult
	if !input.Snapshot.MonitorsArmed {
		return result, budgetHold(selectorFeeEvidenceUnavailable)
	}
	now := time.Now().UTC()
	if input.Now.After(now) || now.Sub(input.Now) > 5*time.Second || input.Snapshot.Slot <= 0 || confirmedSlot < input.Snapshot.Slot || confirmedSlot-input.Snapshot.Slot > observationLagSlots() {
		return result, budgetHold(selectorEvaluationNotCurrent)
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
			return budgetHold(invalidSelectorRouteState)
		}
		now = time.Now().UTC()
		if now.Sub(input.Now) > 5*time.Second {
			return budgetHold(selectorEvaluationNotCurrent)
		}
		input.Now = now
		currentQuotes := make([]MoveQuote, 0, len(input.Quotes))
		for _, q := range input.Quotes {
			if q.currentAtSlot(confirmedSlot) {
				currentQuotes = append(currentQuotes, q)
			}
		}
		input.Quotes = currentQuotes
		var canaryReceipt *pilotCanaryEntryReceipt
		var record bool
		result, canaryReceipt, record, err = selectorSample(input, state.Selector.Result, state.CanaryHistory)
		if err != nil || !record {
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

func selectorSample(input SelectorInput, previous SelectorResult, history map[string]pilotCanaryEntryReceipt) (SelectorResult, *pilotCanaryEntryReceipt, bool, error) {
	if rotationLeft(&previous, input.Snapshot, input.Lanes) {
		return previous, nil, false, nil
	}
	result := SelectOpportunity(input, previous.State)
	if result.Reason == "execution_recovery_or_safety_first" {
		return previous, nil, false, nil
	}
	result, receipt, err := selectPilotCanaryEntry(input, result, history)
	return result, receipt, true, err
}
