package backyard

import "math/big"

// A withdrawal shortfall S = demand - Voltr idle - Squads cash already in
// flight frees E = S + buffer of equity from one lane and keeps the lane at
// its level: one down step withdraws the collateral share C*E/Eq and repays
// only its debt share (the LTV stays where it was), then the freed
// collateral converts to USDC and stages to Voltr. Nothing about the
// withdrawal is stored: each tick's target recomputes the shortfall, and a
// release larger than one step's withdraw room repeats.
const (
	partialWithdrawalBufferBPS     int64 = 100       // 1% of S ...
	partialWithdrawalMinimumBuffer int64 = 1_000_000 // ... at least $1
	// A release of 90% of a lane's equity or more is the operator's exit,
	// not a withdrawal's.
	partialWithdrawalFullExitBPS int64 = 9_000
	// Collateral or cash below this is rounding residue, not a leg.
	partialWithdrawalDustRaw int64 = 1_000
)

// partialWithdrawalReleaseReceipts converts the USDC target E into the
// collateral receipts of its share C*E/Eq at the snapshot's values. Beside
// debt the down step's own withdraw room caps it (planDownStep).
func partialWithdrawalReleaseReceipts(s Snapshot, free int64) int64 {
	equity := s.PositionCollateralValueRaw - s.PositionDebtValueRaw
	if free <= 0 || equity <= 0 || s.PositionCollateralValueRaw <= 0 || s.PositionCollateralRaw <= 0 {
		return 0
	}
	receipts := new(big.Int).Mul(big.NewInt(s.PositionCollateralRaw), big.NewInt(free))
	receipts.Quo(receipts, big.NewInt(equity))
	if !receipts.IsInt64() || receipts.Sign() <= 0 || receipts.Int64() >= s.PositionCollateralRaw {
		return 0
	}
	return receipts.Int64()
}
