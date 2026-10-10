package backyard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
)

// BudgetHold is a pre-send refusal, never an authorization to sign or retry
// an unchanged intent. The caller journals it under the route lease.
type BudgetHold struct {
	Reason  string            `json:"reason"`
	Details map[string]string `json:"details,omitempty"`
	// alreadyJournaled marks a hold that its own send path already recorded
	// durably on the operation row (the pre-broadcast spending-limit
	// refusal). The tick tail must not journal it a second time: the row is
	// already failed, so the store would reject the transition and joining
	// that rejection into the hold would mask it.
	alreadyJournaled bool
}

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
func budgetHold(reason string) error { return &BudgetHold{Reason: reason} }

// journaledBudgetHold constructs a hold whose named reason is already durable
// on the operation row.
func journaledBudgetHold(reason string) error {
	return &BudgetHold{Reason: reason, alreadyJournaled: true}
}

// sanitizedHoldCause keeps a nested hold's reason, or a generic cause for
// transport errors (raw errors may carry RPC URLs).
func sanitizedHoldCause(err error) string {
	var hold *BudgetHold
	if errors.As(err, &hold) {
		return hold.Reason
	}
	return "rpc_read_failed"
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

// Only this marker permits the signed-HOLD expiry path. Invalid persisted
// identities must never release funds using untrusted expiry metadata.
type validatedSignedBudgetHold struct{ hold *BudgetHold }

func (e *validatedSignedBudgetHold) Error() string { return e.hold.Error() }
func (e *validatedSignedBudgetHold) Unwrap() error { return e.hold }

// RecordPhase3BudgetHold preserves a pre-send hold before restart recovery can
// replace it with a generic reason. Only never-submitted states may fail; the
// transition's lease and status CAS protect against concurrent progress.
func (d *Database) RecordPhase3BudgetHold(ctx context.Context, operationID string, hold *BudgetHold) error {
	if d == nil || d.pool == nil || operationID == "" || hold == nil || hold.Reason == "" {
		return fmt.Errorf("invalid budget hold journal input")
	}
	var status OperationStatus
	if err := d.pool.QueryRow(ctx, `SELECT status FROM loyal_yield.multiply_operations WHERE operation_id=$1`, operationID).Scan(&status); err != nil {
		return err
	}
	if status != Decided && status != Built && status != Simulated {
		return budgetHold("budget_hold_requires_never_submitted_operation")
	}
	encoded, err := json.Marshal(hold)
	if err != nil {
		return err
	}
	return d.transition(ctx, operationID, status, Failed,
		`, recovery_reason = $4, expected_effects = jsonb_set(expected_effects, '{budgetHold}', $5::jsonb)`,
		"phase3_budget_hold:"+hold.Reason, string(encoded))
}

// RecordPhase3SignedBudgetHold retains the reason while leaving the signed
// bytes untouched. Merely not recording broadcast intent does not prove a
// signature absent.
func (d *Database) RecordPhase3SignedBudgetHold(ctx context.Context, operationID string, hold *BudgetHold) error {
	if d == nil || d.pool == nil || operationID == "" || hold == nil || hold.Reason == "" {
		return fmt.Errorf("invalid signed budget hold")
	}
	encoded, err := json.Marshal(hold)
	if err != nil {
		return err
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = d.lockOperationLease(ctx, tx, operationID); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET recovery_reason=$2,expected_effects=jsonb_set(expected_effects,'{budgetHold}',$3::jsonb),updated_at=clock_timestamp() WHERE operation_id=$1 AND status='signed'`, operationID, "phase3_budget_hold:"+hold.Reason, string(encoded))
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return budgetHold("signed_budget_hold_state_changed")
	}
	return tx.Commit(ctx)
}
