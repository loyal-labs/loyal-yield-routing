package backyardrwa

import "encoding/json"

const (
	pilotBudgetAuthoritySchema = "voltr-rwa-pilot-budget/v1"
	pilotBudgetAuthorityID     = "01a0a776-cb66-7333-99eb-7e6927c1e114"
	// Reviewed ceilings: $100,000 total deposits and the same working
	// allocation. Widening these code ceilings never widens a budget
	// already activated under the previous limits: its persisted Limits
	// record keeps binding until an explicit operator limit update.
	PilotDepositCapRaw        int64 = 100_000_000_000
	PilotWorkingTrancheCapRaw int64 = 100_000_000_000
	// Stop starting new work after $500 in bounded execution costs. Existing
	// gross exit reservations remain usable, including their reserved fees.
	PilotEntryExecutionCostCapMicros int64 = 500_000_000
)

// This is separate authority, not a reset of the historical goal or spend.
// The durable transition must bind it to a finalized flat-state observation.
// Merely loading a new worker never creates or replaces this record.
type pilotBudgetAuthority struct {
	Schema                  string `json:"schema"`
	AuthorityID             string `json:"authorityId"`
	PreviousBudgetWasAbsent bool   `json:"previousBudgetWasAbsent,omitempty"`
	PreviousBudgetSHA256    string `json:"previousBudgetSha256"`
	Generation              int64  `json:"generation"`
	FinalizedSlot           int64  `json:"finalizedSlot"`
	FlatEvidenceSHA256      string `json:"flatEvidenceSha256"`
}

func (a pilotBudgetAuthority) validate() error {
	if a.Schema != pilotBudgetAuthoritySchema || a.AuthorityID != pilotBudgetAuthorityID || a.Generation < 2 || a.FinalizedSlot <= 0 || !sha256Pattern.MatchString(a.PreviousBudgetSHA256) || !sha256Pattern.MatchString(a.FlatEvidenceSHA256) {
		return budgetHold("invalid_pilot_authority")
	}
	return nil
}
func pilotDeploymentLimits() DeploymentLimits {
	return DeploymentLimits{200_000_000_000, 1_000_000_000_000, 1_500_000_000_000}
}
func (b Phase3Budget) budgetCeiling() DeploymentLimits {
	if b.Pilot != nil {
		return pilotDeploymentLimits()
	}
	return legacyDeploymentLimits()
}
func (b Phase3Budget) budgetFamily(f string) bool {
	return phase3Family(f) || b.Pilot != nil && f == "Prime"
}
func (b Phase3Budget) entryFamily(f string) bool {
	if b.Pilot != nil {
		return f == "Prime" || f == "Maple" || f == "OnRe"
	}
	return phase3Family(f)
}
func (b Phase3Budget) validateExecutionCost(r BudgetReservation) error {
	if b.Pilot == nil {
		if r.ExecutionCostUpperMicros != 0 {
			return budgetHold("execution_cost_requires_pilot_authority")
		}
		return nil
	}
	if !b.entryFamily(r.Family) || r.ExecutionCostUpperMicros <= 0 || r.ExecutionCostUpperMicros > r.UpperMicros {
		return budgetHold("missing_or_invalid_execution_cost_bound")
	}
	return nil
}
func (b Phase3Budget) executionCostSpent() (int64, error) {
	var total int64
	for _, r := range b.Families {
		var err error
		total, err = budgetSum(total, r.ExecutionCostSpentMicros)
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

// Pure half of the explicit transition. The DB boundary supplies finalized
// flat evidence under the route lock and refuses historical unresolved work.
// Copy maps before mutation: a rejected transition never changes the caller.
func activatePilotBudget(prior Phase3Budget, a pilotBudgetAuthority) (Phase3Budget, error) {
	if err := prior.validate(); err != nil {
		return Phase3Budget{}, err
	}
	if prior.Pilot != nil || prior.Closed || len(prior.Reservations) != 0 {
		return Phase3Budget{}, budgetHold("pilot_transition_requires_open_unreserved_legacy_budget")
	}
	encoded, err := json.Marshal(prior)
	if err != nil {
		return Phase3Budget{}, err
	}
	if err = a.validate(); err != nil {
		return Phase3Budget{}, err
	}
	if a.PreviousBudgetWasAbsent {
		for _, row := range prior.Families {
			if row.SpentMicros != 0 || row.ExitMicros != 0 {
				return Phase3Budget{}, budgetHold("absent_prior_budget_has_history")
			}
		}
		encoded = []byte("null")
	}
	if a.PreviousBudgetSHA256 != sha256Bytes(encoded) {
		return Phase3Budget{}, budgetHold("pilot_transition_prior_budget_mismatch")
	}
	next := prior
	next.Families = make(map[string]FamilyBudget, len(prior.Families)+2)
	for family, row := range prior.Families {
		if row.ExitMicros != 0 {
			return Phase3Budget{}, budgetHold("pilot_transition_cannot_clear_exit_reserve")
		}
		next.Families[family] = row
	}
	for _, family := range []string{"Prime", "Maple", "OnRe"} {
		if _, ok := next.Families[family]; !ok {
			next.Families[family] = FamilyBudget{}
		}
	}
	next.Reservations = map[string]BudgetReservation{}
	limits := pilotDeploymentLimits()
	next.Limits = &limits
	next.Pilot = &a
	if err = next.validate(); err != nil {
		return Phase3Budget{}, err
	}
	return next, nil
}
