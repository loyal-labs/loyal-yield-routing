package backyard

const (
	pilotBudgetAuthoritySchema = "voltr-rwa-pilot-budget/v1"
	pilotBudgetAuthorityID     = "01a0a776-cb66-7333-99eb-7e6927c1e114"
	// Stop starting new work once $3,000 of execution cost is spent. Settlement
	// books each operation's realized cost; admission still refuses unless the
	// spent total plus the new work's worst-case bound fits, so the cap is a
	// hard ceiling. Existing gross exit reservations remain usable, including
	// their reserved fees.
	PilotEntryExecutionCostCapMicros int64 = 3_000_000_000
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
		// AUTO is an accounting family of the same goal envelope — phase3Family
		// already tracks it — so pilot entry/cost validation recognizes it like
		// the other funded families. This is local accounting capability, not
		// activation: real trade eligibility stays behind the reviewed manifest
		// binding and the existing selector-entry authority, which still refuse
		// an unbound AUTO lane on every public path.
		return f == "Prime" || f == "Maple" || f == "OnRe" || f == "AUTO"
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
