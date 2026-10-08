package solana

import (
	"errors"
	"math"
	"math/big"
)

type CollateralRaw uint64
type LiquidityRaw uint64
type USDMicros int64

var ErrAmountRange = errors.New("amount outside supported range")

// SQLAmount preserves PostgreSQL BIGINT's narrower range than chain uint64.
func SQLAmount(raw uint64) (int64, error) {
	if raw > math.MaxInt64 {
		return 0, ErrAmountRange
	}
	return int64(raw), nil
}

// RedeemableLiquidity floors an observed exchange ratio; it does not infer one.
func RedeemableLiquidity(raw CollateralRaw, liquidityNumerator, collateralDenominator uint64) (LiquidityRaw, error) {
	if collateralDenominator == 0 {
		return 0, errors.New("missing collateral exchange denominator")
	}
	n := new(big.Int).Mul(new(big.Int).SetUint64(uint64(raw)), new(big.Int).SetUint64(liquidityNumerator))
	n.Quo(n, new(big.Int).SetUint64(collateralDenominator))
	if !n.IsUint64() {
		return 0, ErrAmountRange
	}
	return LiquidityRaw(n.Uint64()), nil
}
