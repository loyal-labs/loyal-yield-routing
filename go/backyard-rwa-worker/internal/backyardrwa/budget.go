package backyardrwa

import (
	"fmt"
	"math"
	"strconv"
)

const (
	Phase3GoalID                     = "01a06b6c-8023-72b1-ad5d-c97c0662820e"
	Phase3TransactionCapMicros int64 = 1_000_000
	Phase3FamilyCapMicros      int64 = 20_000_000
	Phase3GoalCapMicros        int64 = 60_000_000
	// Canary equity is deliberately below the $1 total transaction ceiling:
	// its 1.5x collateral leg is at most $0.75 before fees/price movement.
	// Exact admission still prices every leg; this is not a fee guarantee or
	// a production deposit cap.
	Phase3WorkingTrancheCapRaw int64 = 500_000
)

// BudgetHold is an admission rejection, never an authorization to sign or
// retry an unchanged intent. The caller must journal it under the route lease.
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

func (h *BudgetHold) Error() string  { return "HOLD: " + h.Reason }
func budgetHold(reason string) error { return &BudgetHold{Reason: reason} }

// journaledBudgetHold constructs a hold whose named reason is already durable
// on the operation row.
func journaledBudgetHold(reason string) error {
	return &BudgetHold{Reason: reason, alreadyJournaled: true}
}

// BudgetReservation belongs to one immutable economic intent. UpperMicros
// includes every source debit and fee, not merely the planner's requested size.
// ExitAfterMicros is the complete remaining exit graph after this transaction.
type BudgetReservation struct {
	OperationID              string `json:"operationId"`
	ExecutionCostUpperMicros int64  `json:"executionCostUpperMicros,omitempty"`
	Family                   string `json:"family"`
	IntentSHA256             string `json:"intentSha256"`
	UpperMicros              int64  `json:"upperMicros"`
	ExitAfterMicros          int64  `json:"exitAfterMicros"`
	ExitBeforeMicros         int64  `json:"exitBeforeMicros"`
	Recovery                 bool   `json:"recovery"`
}

type FamilyBudget struct {
	SpentMicros              int64 `json:"spentMicros"`
	ExecutionCostSpentMicros int64 `json:"executionCostSpentMicros,omitempty"`
	ExitMicros               int64 `json:"exitMicros"`
}

// Phase3Budget is persisted in the existing route state. There is deliberately
// no reset, mutable cap, or per-attempt budget constructor. A retry/deploy must
// reload this same goal identity. Database callers serialize changes with the
// existing route-row lock; this pure reducer does not itself provide durability.
type Phase3Budget struct {
	Pilot        *pilotBudgetAuthority        `json:"pilot,omitempty"`
	Limits       *DeploymentLimits            `json:"limits,omitempty"`
	GoalID       string                       `json:"goalId"`
	Closed       bool                         `json:"closed"`
	Families     map[string]FamilyBudget      `json:"families"`
	Reservations map[string]BudgetReservation `json:"reservations"`
}

// The funded families are the three canary lanes plus the manifest-selected
// Maple lane. They share one goal envelope, so the goal cap still bounds the
// whole program; a family row only appears once that lane actually reserves.
func phase3Family(family string) bool {
	return family == "OnRe" || family == "AUTO" || family == "Ethena" || family == "Maple"
}

// This is the finite canary budget scope, not a runtime route registration.
// Sibling substitutions share their family account: retained Prime sibling
// evidence does not authorize an extra funded historical lifecycle.
func phase3BudgetFamilyForLane(lane string) string {
	switch lane {
	case "OnRe/ONyc/USDC", "OnRe/ONyc/USDG", "OnRe/ONyc/USDS":
		return "OnRe"
	case "AUTO/AUTO/PYUSD":
		return "AUTO"
	case "Ethena/USDe/PYUSD":
		return "Ethena"
	case "Prime/PRIME/USDC":
		return "Prime"
	case SelectedRouteID:
		// The manifest-selected lane is part of the funded program. Without it
		// every decision cycled decided -> failed on an unavailable admission
		// snapshot (audit B2) even though its construction evidence is exact.
		return "Maple"
	default:
		return ""
	}
}

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

func (b Phase3Budget) validate() error {
	if err := b.deploymentLimits().validateWithin(b.budgetCeiling()); err != nil {
		return err
	}
	if b.Pilot != nil && b.Pilot.validate() != nil {
		return budgetHold("invalid_pilot_authority")
	}
	maxFamilies := 4
	if b.Pilot != nil {
		maxFamilies = 5
	}
	if b.GoalID != Phase3GoalID || len(b.Families) < 3 || len(b.Families) > maxFamilies || b.Reservations == nil {
		return budgetHold("missing_or_mismatched_goal_budget")
	}
	for family, row := range b.Families {
		if !b.budgetFamily(family) || row.SpentMicros < 0 || row.ExitMicros < 0 || row.ExecutionCostSpentMicros < 0 || row.ExecutionCostSpentMicros > row.SpentMicros || (b.Pilot == nil && row.ExecutionCostSpentMicros != 0) {
			return budgetHold("invalid_family_budget")
		}
	}
	for id, r := range b.Reservations {
		if id == "" || r.OperationID != id || !b.budgetFamily(r.Family) || !sha256Pattern.MatchString(r.IntentSHA256) ||
			r.UpperMicros <= 0 || r.UpperMicros > b.deploymentLimits().TransactionMicros || r.ExitAfterMicros < 0 || r.ExitBeforeMicros < 0 {
			return budgetHold("invalid_persisted_reservation")
		}
		if err := b.validateExecutionCost(r); err != nil {
			return err
		}
	}
	return nil
}

func (b Phase3Budget) totals(family string) (int64, int64, error) {
	var familyTotal, goalTotal int64
	for name, row := range b.Families {
		committedSpend := row.SpentMicros
		if b.Pilot != nil {
			committedSpend = row.ExecutionCostSpentMicros
		}
		total, err := budgetSum(committedSpend, row.ExitMicros)
		if err != nil {
			return 0, 0, err
		}
		goalTotal, err = budgetSum(goalTotal, total)
		if err != nil {
			return 0, 0, err
		}
		if name == family {
			familyTotal = total
		}
	}
	for _, r := range b.Reservations {
		var err error
		goalTotal, err = budgetSum(goalTotal, r.UpperMicros)
		if err != nil {
			return 0, 0, err
		}
		if r.Family == family {
			familyTotal, err = budgetSum(familyTotal, r.UpperMicros)
			if err != nil {
				return 0, 0, err
			}
		}
	}
	return familyTotal, goalTotal, nil
}

// Admit never mutates on rejection. A matching retry reuses, rather than
// replenishes, its reservation. Any other unresolved intent fences the queue.
func (b *Phase3Budget) Admit(r BudgetReservation) error {
	if b == nil {
		return budgetHold("missing_goal_budget")
	}
	if err := b.validate(); err != nil {
		return err
	}
	if b.Closed {
		return budgetHold("goal_envelope_expired")
	}
	if !b.entryFamily(r.Family) || r.OperationID == "" || !sha256Pattern.MatchString(r.IntentSHA256) || r.UpperMicros <= 0 || r.ExitAfterMicros < 0 {
		return budgetHold("invalid_budget_intent")
	}
	if err := b.validateExecutionCost(r); err != nil {
		return err
	}
	if r.UpperMicros > b.deploymentLimits().TransactionMicros {
		return budgetHold("transaction_cap_exceeded")
	}
	if old, ok := b.Reservations[r.OperationID]; ok {
		// Prior reserve is captured by admission, never supplied by a retry.
		old.ExitBeforeMicros = 0
		if old != r {
			return budgetHold("reservation_identity_mismatch")
		}
		return nil
	}
	if len(b.Reservations) != 0 {
		return budgetHold("unresolved_submission_reservation")
	}
	if b.Pilot != nil && !r.Recovery {
		spent, err := b.executionCostSpent()
		if err != nil {
			return err
		}
		next, err := budgetSum(spent, r.ExecutionCostUpperMicros)
		if err != nil {
			return err
		}
		if next > PilotEntryExecutionCostCapMicros {
			return budgetHold("pilot_entry_execution_cost_cap_exhausted")
		}
	}
	row := b.Families[r.Family]
	if r.ExitBeforeMicros != 0 {
		return budgetHold("caller_supplied_prior_exit_reserve")
	}
	r.ExitBeforeMicros = row.ExitMicros
	// An exit may consume its reserve. An entry may not silently reduce an
	// existing exit reserve to make headroom appear available.
	if !r.Recovery && r.ExitAfterMicros < row.ExitMicros {
		return budgetHold("entry_consumes_exit_reserve")
	}
	if r.Recovery {
		needed, err := budgetSum(r.UpperMicros, r.ExitAfterMicros)
		if err != nil {
			return err
		}
		if needed > row.ExitMicros {
			return &BudgetHold{Reason: "recovery_exceeds_reserved_exit", Details: map[string]string{
				"needed": strconv.FormatInt(needed, 10),
				"prior":  strconv.FormatInt(row.ExitMicros, 10),
				"upper":  strconv.FormatInt(r.UpperMicros, 10),
				"tail":   strconv.FormatInt(r.ExitAfterMicros, 10),
			}}
		}
	}
	family, goal, err := b.totals(r.Family)
	if err != nil {
		return err
	}
	// Replace the prior reserve, rather than charging an unwind against both
	// the old reserve and its own transaction. Keep it reserved until settled.
	family, err = budgetSum(family-row.ExitMicros, r.UpperMicros, r.ExitAfterMicros)
	if err != nil {
		return err
	}
	goal, err = budgetSum(goal-row.ExitMicros, r.UpperMicros, r.ExitAfterMicros)
	if err != nil {
		return err
	}
	if family > b.deploymentLimits().FamilyMicros {
		return budgetHold("family_cap_exceeded")
	}
	if goal > b.deploymentLimits().TotalMicros {
		return budgetHold("goal_cap_exceeded")
	}
	row.ExitMicros = r.ExitAfterMicros
	b.Families[r.Family] = row
	b.Reservations[r.OperationID] = r
	return nil
}

// Settle consumes a reservation only after the caller has verified finalized
// effects. Ambiguous outcomes must not call this. Conservative booked spend
// may be the upper bound; a lower actual charge needs independent valuation.
func (b *Phase3Budget) Settle(operationID, intentSHA256 string, actualMicros int64) error {
	if b == nil {
		return budgetHold("missing_goal_budget")
	}
	if err := b.validate(); err != nil {
		return err
	}
	r, ok := b.Reservations[operationID]
	if !ok || r.IntentSHA256 != intentSHA256 {
		return budgetHold("reservation_identity_mismatch")
	}
	if actualMicros < 0 || actualMicros > r.UpperMicros {
		return budgetHold("reconciled_spend_exceeds_reservation")
	}
	row := b.Families[r.Family]
	spent, err := budgetSum(row.SpentMicros, actualMicros)
	if err != nil {
		return err
	}
	if b.Pilot != nil {
		if actualMicros != r.UpperMicros {
			return budgetHold("pilot_settlement_requires_full_admitted_bound")
		}
		expense, err := budgetSum(row.ExecutionCostSpentMicros, r.ExecutionCostUpperMicros)
		if err != nil {
			return err
		}
		row.ExecutionCostSpentMicros = expense
	}
	row.SpentMicros = spent
	b.Families[r.Family] = row
	delete(b.Reservations, operationID)
	return nil
}

// releaseUnspent restores the pre-transaction exit reserve, not the smaller
// post-exit estimate. Only the journal's proven-never-submitted or
// expired-and-absent terminal transition may call it. Ambiguity is not proof.
func (b *Phase3Budget) releaseUnspent(operationID, intentSHA256 string) error {
	if b == nil {
		return budgetHold("missing_goal_budget")
	}
	if err := b.validate(); err != nil {
		return err
	}
	r, ok := b.Reservations[operationID]
	if !ok || r.IntentSHA256 != intentSHA256 {
		return budgetHold("reservation_identity_mismatch")
	}
	row := b.Families[r.Family]
	if row.ExitMicros != r.ExitAfterMicros {
		return budgetHold("exit_reserve_state_mismatch")
	}
	row.ExitMicros = r.ExitBeforeMicros
	b.Families[r.Family] = row
	delete(b.Reservations, operationID)
	return nil
}

func (b Phase3Budget) AuthorizeIntent(operationID, intentSHA256 string) error {
	if err := b.validate(); err != nil {
		return err
	}
	if b.Closed {
		return budgetHold("goal_envelope_expired")
	}
	r, ok := b.Reservations[operationID]
	if !ok || r.IntentSHA256 != intentSHA256 {
		return budgetHold("unreserved_transaction")
	}
	family, goal, err := b.totals(r.Family)
	if err != nil {
		return err
	}
	if family > b.deploymentLimits().FamilyMicros || goal > b.deploymentLimits().TotalMicros {
		return budgetHold("persisted_budget_exceeds_cap")
	}
	return nil
}

// ValueDebitMicros uses an upper-bound USDC price and ceiling rounding for
// source debits and fees. Price freshness and source identity are established
// by observation; this function only performs exact, overflow-checked math.
func ValueDebitMicros(raw uint64, decimals uint8, upperPriceMicros uint64) (int64, error) {
	value, err := ValueRawUSDC(raw, decimals, upperPriceMicros, true)
	if err != nil {
		return 0, fmt.Errorf("debit valuation: %w", err)
	}
	return value, nil
}
