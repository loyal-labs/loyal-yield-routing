package backyard

import (
	"math"
	"math/big"
)

const (
	leverageUpReason  = "leverage_up"
	leverageUpNearBPS = 200
	// Smallest leverage_up borrow in debt raw units (six-decimal stables).
	leverageMinimumBorrowRaw   = 10_000_000
	leverageSwapLossBPS        = 100
	leverageUpRefreshMarginBPS = 10
	leverageUpKeepScale        = 100_000_000
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
			return 0
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

// leverageUpCapFee caps receive so the step lands within ceilingBPS after the swap loss; below the minimum borrow it refuses.
func leverageUpCapFee(p KaminoPosition, ceilingBPS int64, receive uint64, fee func(uint64) (uint64, error)) (uint64, error) {
	collateral, err := p.collateralValueDebtRaw()
	if err != nil {
		return 0, err
	}
	ceiling := new(big.Int).Mul(new(big.Int).SetUint64(collateral), big.NewInt(ceilingBPS))
	ceiling.Quo(ceiling, big.NewInt(10_000))
	refreshMargin := new(big.Int).Mul(ceiling, big.NewInt(leverageUpRefreshMarginBPS))
	refreshMargin.Quo(refreshMargin, big.NewInt(10_000))
	debtGrowthRoom := new(big.Int).Sub(ceiling, new(big.Int).SetUint64(p.DebtRaw))
	debtGrowthRoom.Sub(debtGrowthRoom, refreshMargin)
	if debtGrowthRoom.Sign() <= 0 {
		return 0, budgetHold("leverage_up_no_ltv_room")
	}
	keep := big.NewInt(leverageUpKeepScale - ceilingBPS*(10_000-leverageSwapLossBPS))
	if keep.Sign() <= 0 {
		return 0, budgetHold("leverage_up_no_ltv_room")
	}
	receive, err = receiveWithinGrowthRoom(receive, debtGrowthRoom, keep, fee)
	if err != nil {
		return 0, err
	}
	if receive < leverageMinimumBorrowRaw {
		return 0, budgetHold("leverage_up_below_minimum_borrow")
	}
	return receive, nil
}

func receiveWithinGrowthRoom(receive uint64, debtGrowthRoom, keep *big.Int, fee func(uint64) (uint64, error)) (uint64, error) {
	room := new(big.Int).Mul(debtGrowthRoom, big.NewInt(leverageUpKeepScale))
	room.Quo(room, keep)
	if room.IsUint64() {
		receive = min(receive, room.Uint64())
	}
	f, err := fee(receive)
	if err != nil {
		return 0, err
	}
	used := new(big.Int).Mul(new(big.Int).SetUint64(receive), keep)
	used.Quo(used, big.NewInt(leverageUpKeepScale))
	used.Add(used, new(big.Int).SetUint64(f))
	if used.Cmp(debtGrowthRoom) > 0 {
		left := new(big.Int).Sub(debtGrowthRoom, new(big.Int).SetUint64(f))
		if left.Sign() <= 0 {
			return 0, budgetHold("leverage_up_no_ltv_room")
		}
		left.Mul(left, big.NewInt(leverageUpKeepScale)).Quo(left, keep)
		receive = min(receive, left.Uint64())
	}
	return receive, nil
}

func heldLevelLTVBPS(s Snapshot) int64 {
	return leverageLevelLTVBPS(heldLevel(s.RouteLane, max(s.LeverageTargetLevel, firstLeveredLevel)))
}

func hardLTVStepLTVBPS(s Snapshot) int64 {
	hard, ltv := hardLTVBPS(s.LiquidationThresholdBPS, s.MaxLTVBPS), int64(0)
	for _, r := range leverageLadder {
		if l := leverageLevelLTVBPS(r.level); l < hard {
			ltv = l
		}
	}
	return min(heldLevelLTVBPS(s), ltv)
}

const leverageDownPartialReason = "leverage_down_partial"
