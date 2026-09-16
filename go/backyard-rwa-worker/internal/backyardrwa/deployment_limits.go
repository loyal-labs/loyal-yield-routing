package backyardrwa

import "fmt"

// DeploymentLimits are carried inside the existing budget record. The original
// goal identity, spent principal and reserved exits remain authoritative. These
// settings may narrow the reviewed envelope; they never replenish it or extend
// its scope. Larger limits require a separately reviewed authority transition.
type DeploymentLimits struct {
	TransactionMicros int64 `json:"transactionMicros"`
	FamilyMicros      int64 `json:"familyMicros"`
	TotalMicros       int64 `json:"totalMicros"`
}

func legacyDeploymentLimits() DeploymentLimits {
	return DeploymentLimits{Phase3TransactionCapMicros, Phase3FamilyCapMicros, Phase3GoalCapMicros}
}
func (l DeploymentLimits) validate() error {
	ceiling := legacyDeploymentLimits()
	if l.TransactionMicros <= 0 || l.FamilyMicros < l.TransactionMicros || l.TotalMicros < l.FamilyMicros || l.TransactionMicros > ceiling.TransactionMicros || l.FamilyMicros > ceiling.FamilyMicros || l.TotalMicros > ceiling.TotalMicros {
		return fmt.Errorf("deployment_limits_exceed_reviewed_envelope")
	}
	return nil
}
func (b Phase3Budget) deploymentLimits() DeploymentLimits {
	if b.Limits != nil {
		return *b.Limits
	}
	return legacyDeploymentLimits()
}

// ConstrainLimits never changes spend, scope, expiry, or reservations. Callers
// persist the same budget under the existing route lock, not a replacement row.
func (b *Phase3Budget) ConstrainLimits(l DeploymentLimits) error {
	if b == nil {
		return budgetHold("missing_goal_budget")
	}
	if err := b.validate(); err != nil {
		return err
	}
	if err := l.validate(); err != nil {
		return err
	}
	old := b.deploymentLimits()
	if l.TransactionMicros > old.TransactionMicros || l.FamilyMicros > old.FamilyMicros || l.TotalMicros > old.TotalMicros {
		return budgetHold("deployment_limit_increase_requires_authority_transition")
	}
	if len(b.Reservations) != 0 {
		return budgetHold("deployment_limits_have_unresolved_reservations")
	}
	for family := range b.Families {
		used, total, err := b.totals(family)
		if err != nil {
			return err
		}
		if used > l.FamilyMicros || total > l.TotalMicros {
			return budgetHold("deployment_limits_below_committed_spending")
		}
	}
	b.Limits = &l
	return nil
}
