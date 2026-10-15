package backyard

import (
	"math"
	"math/big"
)

// B2 up moves. The desired target bounds a same-batch capacity-sized loan.
// One economically approved raw receive is journal-bound to one operation.
// The uncapped desired amount is (L-1)*C - L*D in debt units (1x->1.5x:
// C/2; 1.5x->1.75x: equity/4; 2x->2.5x: equity/2). The step is one
// transaction (docs/backyard-atomic-step.md): the borrow plus its fee lands,
// after the conversion's 1% swap loss and the redeposit, within the lane
// cap's LTV, and KLend's own loan-to-value bounds the borrow before the
// redeposit.
const (
	leverageUpReason = "leverage_up"
	// A position within 2 LTV points of a level counts as at that level.
	leverageUpNearBPS = 200
	// Smallest leverage_up borrow in debt raw units (six-decimal stables).
	leverageMinimumBorrowRaw = 10_000_000
	leverageSwapLossBPS      = 100
)

// leverageUpLevel is the next level above the position (at most one step),
// when the stored level and the lane's cap allow it; 0 means no up move. An
// up step sizes from the same batch (LeverageBorrowRaw) once the selector's
// economics approve it (target.go).
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
	for _, r := range leverageLadder[1:] {
		if leverageLevelLTVBPS(r.level) > ltv+leverageUpNearBPS {
			if r.level > s.LeverageTargetLevel || r.level > laneMaxLevel(s.RouteLane) {
				return 0
			}
			return r.level
		}
	}
	return 0
}

// leverageUpBorrowRaw sizes the borrow toward level, any level above 1 on
// the ladder, before the fee and the lane's ceiling; see leverageUpCapFee.
func (p KaminoPosition) leverageUpBorrowRaw(level float64) (uint64, error) {
	if i, ok := leverageRungIndex(level); !ok || i == 0 {
		return 0, budgetHold("invalid_leverage_up_level")
	}
	collateral, err := p.collateralValueDebtRaw()
	if err != nil {
		return 0, err
	}
	hundredths := int64(math.Round(level * 100))
	b := new(big.Int).Mul(new(big.Int).SetUint64(collateral), big.NewInt(hundredths-100))
	b.Sub(b, new(big.Int).Mul(new(big.Int).SetUint64(p.DebtRaw), big.NewInt(hundredths)))
	b.Quo(b, big.NewInt(100))
	if b.Sign() <= 0 || !b.IsUint64() {
		return 0, budgetHold("leverage_up_level_already_reached")
	}
	return b.Uint64(), nil
}

// leverageUpCapFee keeps the step's landing within ceilingBPS: debt +
// receive + fee over collateral + receive less the swap loss, with a 0.1%
// margin across the prepare-to-simulation price refresh. It refuses a borrow
// below the minimum.
func leverageUpCapFee(p KaminoPosition, ceilingBPS int64, receive uint64, fee func(uint64) (uint64, error)) (uint64, error) {
	collateral, err := p.collateralValueDebtRaw()
	if err != nil {
		return 0, err
	}
	ceiling := new(big.Int).Mul(new(big.Int).SetUint64(collateral), big.NewInt(ceilingBPS))
	ceiling.Quo(ceiling, big.NewInt(10_000))
	// x is what debt may still grow by before the redeposit counts: the
	// ceiling, less the debt and the margin.
	x := new(big.Int).Sub(ceiling, new(big.Int).SetUint64(p.DebtRaw))
	x.Sub(x, new(big.Int).Quo(ceiling, big.NewInt(1000)))
	if x.Sign() <= 0 {
		return 0, budgetHold("leverage_up_no_ltv_room")
	}
	// Each received unit adds itself to debt and (1 - loss) of itself, at
	// the ceiling's LTV, to the room: r*(1-k) + fee <= x.
	const scale = 100_000_000
	keep := big.NewInt(scale - ceilingBPS*(10_000-leverageSwapLossBPS))
	if keep.Sign() <= 0 {
		return 0, budgetHold("leverage_up_no_ltv_room")
	}
	room := new(big.Int).Mul(x, big.NewInt(scale))
	room.Quo(room, keep)
	if room.IsUint64() {
		receive = min(receive, room.Uint64())
	}
	f, err := fee(receive)
	if err != nil {
		return 0, err
	}
	used := new(big.Int).Mul(new(big.Int).SetUint64(receive), keep)
	used.Quo(used, big.NewInt(scale))
	used.Add(used, new(big.Int).SetUint64(f))
	if used.Cmp(x) > 0 {
		left := new(big.Int).Sub(x, new(big.Int).SetUint64(f))
		if left.Sign() <= 0 {
			return 0, budgetHold("leverage_up_no_ltv_room")
		}
		left.Mul(left, big.NewInt(scale)).Quo(left, keep)
		receive = min(receive, left.Uint64())
	}
	if receive < leverageMinimumBorrowRaw {
		return 0, budgetHold("leverage_up_below_minimum_borrow")
	}
	return receive, nil
}

// heldLevelLTVBPS is the LTV a down step (hard LTV, or a stored level below
// the position) brings the lane to: its held level's, at least 1.5x.
func heldLevelLTVBPS(s Snapshot) int64 {
	return leverageLevelLTVBPS(heldLevel(s.RouteLane, max(s.LeverageTargetLevel, 1.5)))
}

// hardLTVStepLTVBPS is the LTV a hard-LTV step brings the lane to: its held
// level's, or, when the brake has fallen to or under that, the highest
// level's under the brake (debt 0 under 1.5x's).
func hardLTVStepLTVBPS(s Snapshot) int64 {
	hard, ltv := hardLTVBPS(s.LiquidationThresholdBPS, s.MaxLTVBPS), int64(0)
	for _, r := range leverageLadder {
		if l := leverageLevelLTVBPS(r.level); l < hard {
			ltv = l
		}
	}
	return min(heldLevelLTVBPS(s), ltv)
}

// leverageDownPartialReason is the down step to the held level's LTV (a
// stored level, or the lane's cap, below a position above that level's
// band).
const leverageDownPartialReason = "leverage_down_partial"
