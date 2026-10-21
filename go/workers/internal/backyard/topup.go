package backyard

const (
	topupSwapReason       = "topup_usdc_requires_collateral"
	topupAllocationReason = "topup_voltr_idle"
	topupDepositReason    = "topup_collateral_deposit"
	stepResidueReason     = "step_residue_to_collateral"
)

func laneSupportsSwap(lane string, action Action) bool {
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
