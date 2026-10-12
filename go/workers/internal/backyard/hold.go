package backyard

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// The holds a tick ends in, the closed set loyal_backyard_hold carries:
//
//	at_target       the vault is where target() puts it
//	no_room         the target wants a move the chain has no room for
//	stale           an input is not current or could not be read; the next
//	                tick observes again
//	chain_refused   the signed simulation of the decided transaction failed
//	stopped         the operator's manual recovery latch
//
// Why, in detail, goes in the hold's Details ("cause", "simulation") and the
// log line, never in the reason.
const (
	holdAtTarget     = "at_target"
	holdNoRoom       = "no_room"
	holdStale        = "stale"
	holdChainRefused = "chain_refused"
	holdStopped      = "stopped"
)

// BudgetHold is a pre-send refusal, never an authorization to sign or retry
// an unchanged intent. Its Reason is one of the hold enum; a producer that
// names only its check (budgetHold) holds stale with that check as its cause.
type BudgetHold struct {
	Reason  string            `json:"reason"`
	Details map[string]string `json:"details,omitempty"`
	cause   error
}

// Unwrap is the error the hold was taken on, if any.
func (h *BudgetHold) Unwrap() error { return h.cause }

func (h *BudgetHold) Error() string {
	if len(h.Details) == 0 {
		return "HOLD: " + h.Reason
	}
	keys := make([]string, 0, len(h.Details))
	for k := range h.Details {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	text := "HOLD: " + h.Reason
	for _, k := range keys {
		text += " " + k + "=" + h.Details[k]
	}
	return text
}

// budgetHold is the stale hold of a check named cause.
func budgetHold(cause string) error { return holdOf(holdStale, cause, nil) }

// budgetHoldFrom is budgetHold taken on err.
func budgetHoldFrom(err error, cause string) error { return holdOf(holdStale, cause, err) }

// roomHold is the no_room hold of a check named cause, taken on err.
func roomHold(cause string, err error) error { return holdOf(holdNoRoom, cause, err) }

func holdOf(reason, cause string, err error) error {
	return &BudgetHold{Reason: reason, Details: map[string]string{"cause": cause}, cause: err}
}

// holdCause is a hold's detail: the check it names, or its simulation error.
func holdCause(h *BudgetHold) string {
	if c := h.Details["cause"]; c != "" {
		return c
	}
	return h.Details["simulation"]
}

// budgetSum adds non-negative micros, refusing overflow.
func budgetSum(values ...int64) (int64, error) {
	var total int64
	for _, value := range values {
		if value < 0 || total > math.MaxInt64-value {
			return 0, budgetHold("invalid_budget_accounting")
		}
		total += value
	}
	return total, nil
}

// RecordBudgetHold fails the tick's never-submitted operation with its hold:
// recovery_reason hold:<reason>, the hold in its expected effects. Only
// never-submitted states may fail.
func (d *Database) RecordBudgetHold(ctx context.Context, operationID string, hold *BudgetHold) error {
	if d == nil || d.pool == nil || operationID == "" || hold == nil || hold.Reason == "" {
		return fmt.Errorf("invalid budget hold journal input")
	}
	var status OperationStatus
	if err := d.pool.QueryRow(ctx, `SELECT status FROM loyal_yield.multiply_operations WHERE operation_id=$1`, operationID).Scan(&status); err != nil {
		return err
	}
	if status != Decided && status != Built && status != Simulated {
		return fmt.Errorf("budget hold requires a never-submitted operation, not %s", status)
	}
	encoded, err := json.Marshal(hold)
	if err != nil {
		return err
	}
	return d.transition(ctx, operationID, status, Failed,
		`, recovery_reason = $4, expected_effects = jsonb_set(expected_effects, '{budgetHold}', $5::jsonb)`,
		"hold:"+hold.Reason, string(encoded))
}
