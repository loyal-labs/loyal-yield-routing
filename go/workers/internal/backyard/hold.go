package backyard

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// Why, in detail, goes in the hold's Details ("cause", "simulation") and the
// log line, never in the reason.
type HoldReason string

const (
	holdNone         HoldReason = ""
	holdAtTarget     HoldReason = "at_target"
	holdNoRoom       HoldReason = "no_room"
	holdStale        HoldReason = "stale"
	holdChainRefused HoldReason = "chain_refused"
	holdStopped      HoldReason = "stopped"
)

type PreSendHold struct {
	Reason  HoldReason        `json:"reason"`
	Details map[string]string `json:"details,omitempty"`
	cause   error
}

func (h *PreSendHold) Unwrap() error { return h.cause }

func (h *PreSendHold) Error() string {
	if len(h.Details) == 0 {
		return "HOLD: " + string(h.Reason)
	}
	keys := make([]string, 0, len(h.Details))
	for k := range h.Details {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	text := "HOLD: " + string(h.Reason)
	for _, k := range keys {
		text += " " + k + "=" + h.Details[k]
	}
	return text
}

func staleHold(cause string) error { return holdOf(holdStale, cause, nil) }

func staleHoldFrom(err error, cause string) error { return holdOf(holdStale, cause, err) }

func roomHold(cause string, err error) error { return holdOf(holdNoRoom, cause, err) }

func holdOf(reason HoldReason, cause string, err error) error {
	return &PreSendHold{Reason: reason, Details: map[string]string{"cause": cause}, cause: err}
}

func holdCause(h *PreSendHold) string {
	if c := h.Details["cause"]; c != "" {
		return c
	}
	return h.Details["simulation"]
}

func (d *Database) RecordPreSendHold(ctx context.Context, operationID string, hold *PreSendHold) error {
	if d == nil || d.pool == nil || operationID == "" || hold == nil || hold.Reason == holdNone {
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
		"hold:"+string(hold.Reason), string(encoded))
}
