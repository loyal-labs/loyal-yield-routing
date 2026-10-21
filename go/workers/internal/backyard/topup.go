package backyard

const (
	topupSwapReason       = "topup_usdc_requires_collateral"
	topupAllocationReason = "topup_voltr_idle"
	topupDepositReason    = "topup_collateral_deposit"
	stepResidueReason     = "step_residue_to_collateral"
)

// laneSwap reads the edge table the builder signs from, so a step never picks
// a swap the lane's policy lacks (AUTO and OnRe have no USDC->debt edge).
func laneSwap(lane string, action Action) bool {
	if _, hastra := hastraConversionLeg(action, lane); hastra {
		return true
	}
	if action == SwapDebtToUSDCStep && !debtToUSDCConvertible(lane) {
		return false
	}
	if catalogJupiterRoute(lane) {
		_, _, err := catalogEdge(action, lane)
		return err == nil
	}
	return basicLane(lane) && basicSwapAction(action)
}
