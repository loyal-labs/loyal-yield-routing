package backyard

import "math/big"

// B2 up moves. The desired target bounds a same-batch capacity-sized loan.
// One economically approved raw receive is journal-bound to one operation.
// The uncapped desired amount is
// (L-1)*C - L*D in debt units (1x->1.5x: C/2; 1.5x->1.75x: equity/4). The
// borrow plus its fee never takes the instant LTV above 50%, and the loop
// must land at or below 45% even after a 1% swap loss.
const (
	leverageUpReason = "leverage_up"
	// A position within 2 LTV points of a level counts as at that level.
	leverageUpNearBPS = 200
	// Smallest leverage_up borrow in debt raw units (six-decimal stables).
	leverageMinimumBorrowRaw = 10_000_000
	leverageSwapLossBPS      = 100
	// leverageMaxLiveLevel caps live levels.
	leverageMaxLiveLevel = 1.75
)

// leverageUpLevel is the next level above the position (at most one step),
// when the stored level allows it; 0 means no up move. An up step sizes from
// the same batch (LeverageBorrow150Raw/175Raw) once the selector's economics
// approve it (target.go).
func leverageUpLevel(s Snapshot) float64 {
	if !earnActiveLane(s.RouteLane) || s.LeverageTargetLevel <= 1 {
		return 0
	}
	ltv := int64(0)
	if s.PositionDebtRaw > 0 {
		if s.LTVBPS <= 0 {
			return 0 // debt without an observed LTV: never lever up
		}
		ltv = s.LTVBPS
	}
	for _, level := range leverageLevels[1:] {
		if leverageLevelLTVBPS(level) > ltv+leverageUpNearBPS {
			if level > s.LeverageTargetLevel || level > leverageMaxLiveLevel {
				return 0
			}
			return level
		}
	}
	return 0
}

// leverageUpBorrowRaw sizes the borrow toward levelHundredths (150 or 175)
// before the fee cap; see leverageUpCapFee.
func (p KaminoPosition) leverageUpBorrowRaw(levelHundredths int64) (uint64, error) {
	if levelHundredths != 150 && levelHundredths != 175 {
		return 0, budgetHold("invalid_leverage_up_level")
	}
	half, err := p.targetLTVBorrowRaw()
	if err != nil {
		return 0, err
	}
	c := new(big.Int).Lsh(new(big.Int).SetUint64(half), 1)
	b := new(big.Int).Mul(c, big.NewInt(levelHundredths-100))
	b.Sub(b, new(big.Int).Mul(new(big.Int).SetUint64(p.DebtRaw), big.NewInt(levelHundredths)))
	b.Quo(b, big.NewInt(100))
	if b.Sign() <= 0 || !b.IsUint64() {
		return 0, budgetHold("leverage_up_level_already_reached")
	}
	return b.Uint64(), nil
}

// leverageUpCapFee keeps debt + receive + fee within 50% of collateral and
// refuses a borrow below the minimum.
func leverageUpCapFee(p KaminoPosition, receive uint64, fee func(uint64) (uint64, error)) (uint64, error) {
	half, err := p.targetLTVBorrowRaw()
	if err != nil {
		return 0, err
	}
	if half <= p.DebtRaw {
		return 0, budgetHold("leverage_up_no_instant_ltv_room")
	}
	// 0.1% margin keeps the instant LTV under 50% across the
	// prepare-to-simulation price refresh.
	room := half - p.DebtRaw - half/1000
	if room == 0 || room > half {
		return 0, budgetHold("leverage_up_no_instant_ltv_room")
	}
	receive = min(receive, room)
	f, err := fee(receive)
	if err != nil {
		return 0, err
	}
	if receive+f > room {
		if f >= room {
			return 0, budgetHold("leverage_up_no_instant_ltv_room")
		}
		receive = room - f
	}
	if receive < leverageMinimumBorrowRaw {
		return 0, budgetHold("leverage_up_below_minimum_borrow")
	}
	return receive, nil
}

// hardLTVTargetBPS is the LTV a hard-LTV step brings the position to: its
// stored level's, between 1.5x and the live cap.
func hardLTVTargetBPS(s Snapshot) int64 {
	return leverageLevelLTVBPS(min(max(s.LeverageTargetLevel, 1.5), leverageMaxLiveLevel))
}

// leverageDownPartialReason is the down step to the 1.5x LTV (a stored 1.5x
// or 1x level below a position above the 1.5x band).
const leverageDownPartialReason = "leverage_down_partial"
