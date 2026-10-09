package backyard

import "time"

// A verified partial-risk operation may spend only its own admitted input.
// It must not create a reusable full-debt-clear authority in route state.
func bindPartialRiskAuthorization(m RouteManifest, request any, effects ExpectedEffects, plan *phase3BridgeAdmission, auth *phase3OperationAuthorization, state debtClearRouteState, proof *debtClearRiskProof, operationID string, slot int64, admission bool) (bool, error) {
	partial := false
	if plan != nil {
		switch r := request.(type) {
		case KaminoPrimeUSDCRequest:
			partial = autoEmergencyPartialRepayment(plan.Snapshot, plan.Decision)
			if partial {
				if r.FullPayoff || r.RepaymentRelease || r.RouteLane != autoAUTOPYUSD.Lane || r.Action != DeleverRouteStep || r.AmountRaw != uint64(plan.Decision.AmountRaw) || plan.RepaymentProjection == nil || plan.Payoff == nil {
					return true, budgetHold("partial_risk_input_changed")
				}
				if _, err := validatePartialRepaymentProjection(r, effects, plan.Snapshot, *plan.RepaymentProjection); err != nil {
					return true, err
				}
			}
		case JupiterSwapRequest:
			partial = r.EmergencyTopupFunding
			if partial {
				if err := validateEmergencyTopupFundingProof(m, plan); err != nil {
					return true, err
				}
			}
		}
	}
	if !partial {
		if auth.PartialRisk != nil {
			return true, budgetHold("partial_risk_input_changed")
		}
		return false, nil
	}
	if auth.DebtClear != nil || state.Authority != nil || state.Unwind != nil || plan.Snapshot.Unwind || plan.Snapshot.CutoverDrain {
		return true, budgetHold("partial_risk_full_exit_conflict")
	}
	if !admission {
		proof = auth.PartialRisk
	}
	if proof == nil || slot < proof.Slot || slot-proof.Slot > observationLagSlots() || slot > plan.ValidThroughSlot || !freshAt(time.Now().UTC(), proof.ObservedAt, 30*time.Second) || !topupRiskHandoff(plan, proof, operationID) || !decisionsEqual(m.DecideOnManifest(plan.Snapshot), plan.Decision) {
		return true, budgetHold("partial_risk_authority_unavailable")
	}
	copied := *proof
	auth.PartialRisk = &copied
	return true, nil
}
