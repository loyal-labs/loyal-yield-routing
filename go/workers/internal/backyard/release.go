package backyard

import "math/big"

const (
	partialWithdrawalBufferBPS        int64 = 100
	partialWithdrawalMinimumBufferRaw int64 = bridgeUSDCDollarRaw
	partialWithdrawalFullExitBPS      int64 = 9_000
	partialWithdrawalDustRaw          int64 = 1_000
	bridgeUSDCDollarRaw               int64 = 1_000_000
)

func partialWithdrawalReleaseReceipts(s Snapshot, releaseValueRaw int64) (collateralReceiptsRaw int64) {
	equity := s.PositionCollateralValueRaw - s.PositionDebtValueRaw
	if releaseValueRaw <= 0 || equity <= 0 || s.PositionCollateralValueRaw <= 0 || s.PositionCollateralRaw <= 0 {
		return 0
	}
	receipts := new(big.Int).Mul(big.NewInt(s.PositionCollateralRaw), big.NewInt(releaseValueRaw))
	receipts.Quo(receipts, big.NewInt(equity))
	if !receipts.IsInt64() || receipts.Sign() <= 0 || receipts.Int64() >= s.PositionCollateralRaw {
		return 0
	}
	return receipts.Int64()
}
