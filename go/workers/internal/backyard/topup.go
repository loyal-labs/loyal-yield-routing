package backyard

// Journaled reasons of a top-up beside a funded position. A step's close
// moves its conversion's minimum output; the excess the conversion delivers
// stays beside the position: borrowed-token excess converts to collateral,
// which the top-up deposit then joins.
const (
	topupSwapReason       = "topup_usdc_requires_collateral"
	topupAllocationReason = "topup_voltr_idle"
	topupDepositReason    = "topup_collateral_deposit"
	stepResidueReason     = "step_residue_to_collateral"
)

// A top-up allocation below this is not worth its fees; the cash waits in
// Voltr for the next deposit. ponytail: fixed $10 floor, derive it from the
// measured leg costs if small deposits matter.
const topupMinimumRaw int64 = 10_000_000

// debtTopupLane: a top-up also runs beside a debt-bearing position where
// debt custody is not bridge cash (AUTO/PYUSD). On a shared-USDC-debt lane
// Squads cash beside debt is a step's excess, converted as residue.
func debtTopupLane(lane string) bool {
	return earnActiveLane(lane) && !sharedUSDCDebt(lane)
}

// laneSwap reads the edge table the builder signs from, so a step never picks
// a swap the lane's policy lacks (AUTO and OnRe have no USDC->debt edge).
func laneSwap(lane string, action Action) bool {
	if _, hastra := hastraConversionLeg(action, lane); hastra {
		return true
	}
	if action == SwapDebtToUSDCStep && !debtToUSDCConvertible(lane) {
		return false
	}
	_, _, _, _, err := jupiterEdgeForRoute(action, lane)
	return err == nil
}
