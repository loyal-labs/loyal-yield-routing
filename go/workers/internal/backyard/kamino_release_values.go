package backyard

import "math/big"

// The selector prices a destination's later exit (selector_destination_exit.go)
// with the protocol's release room: how much collateral a position may
// withdraw before its debt is repaid, inside the market's risk limits.

// These values contain no receipt count or account image. Callers supply a
// conservative underlying collateral amount and a finite-window debt bound.
type kaminoReleaseValues struct {
	CollateralRaw      uint64
	DebtRaw            uint64
	CollateralDecimals uint8
	DebtDecimals       uint8
	CollateralPriceSF  [16]byte
	DebtPriceSF        [16]byte
}

// Limits come from validated market/reserve settings. Group zero, effective
// borrow factor 100 and non-emergency mode are caller prerequisites; this pure
// arithmetic helper does not confer execution authority or validate accounts.
type kaminoPilotReleaseLimits struct {
	MaxLTVPct                byte
	LiquidationPct           byte
	GlobalAllowedBorrowValue uint64
	MinimumRemainingValueSF  [16]byte
}

func releaseValuesForPosition(position KaminoPosition) kaminoReleaseValues {
	return kaminoReleaseValues{CollateralRaw: position.RedeemablePrimeRaw, DebtRaw: position.DebtRaw,
		CollateralDecimals: position.CollateralDecimals, DebtDecimals: position.DebtDecimals,
		CollateralPriceSF: position.CollateralPriceSF, DebtPriceSF: position.DebtPriceSF}
}

func (limits kaminoPilotReleaseLimits) ceilingBPS() (uint64, error) {
	maxLTV := int64(limits.MaxLTVPct) * 100
	hard := min(int64(limits.LiquidationPct)*100-1500, 6000)
	ceiling := min(int64(5500), maxLTV-500, hard-500)
	if maxLTV <= 0 || maxLTV >= int64(limits.LiquidationPct)*100 || limits.LiquidationPct > 100 || ceiling <= TargetLTVBPS {
		return 0, budgetHold("pilot_release_risk_margin_unavailable")
	}
	return uint64(ceiling), nil
}

// Bound protocol/risk room in underlying units. A positive result alone does
// not establish DEX liquidity or a full-payoff quote. Actual execution converts
// this allowance through the observed receipt exchange rate before release.
func pilotRepaymentLiquidityAllowanceForValues(values kaminoReleaseValues, limits kaminoPilotReleaseLimits) (uint64, error) {
	ceiling, err := limits.ceilingBPS()
	if err != nil {
		return 0, err
	}
	allowance, err := withdrawableUnderlyingAtLTV(values, ceiling)
	if err != nil {
		return 0, err
	}
	// Recompute dollar values (scaled by 2^60) from current reserve prices.
	// Reserves may be newer than the obligation's cached valuation. Round the
	// finite-window debt up and redeemable collateral down before comparing.
	futureDebt := new(big.Int).Mul(new(big.Int).SetUint64(values.DebtRaw), littleInt(values.DebtPriceSF[:]))
	scale := new(big.Int).Exp(big.NewInt(10), new(big.Int).SetUint64(uint64(values.DebtDecimals)), nil)
	futureDebt.Add(futureDebt, new(big.Int).Sub(new(big.Int).Set(scale), big.NewInt(1))).Quo(futureDebt, scale)
	price := littleInt(values.CollateralPriceSF[:])
	if futureDebt.Sign() <= 0 || price.Sign() <= 0 {
		return 0, budgetHold("pilot_release_protocol_allowance_unavailable")
	}
	collateralScale := new(big.Int).Exp(big.NewInt(10), new(big.Int).SetUint64(uint64(values.CollateralDecimals)), nil)
	// KLend checks the remaining collateral asset against the market minimum
	// after withdrawal. Round required underlying up, retaining one raw unit
	// for its fractional receipt-to-liquidity comparison.
	minimum := new(big.Int).Mul(littleInt(limits.MinimumRemainingValueSF[:]), collateralScale)
	minimum.Add(minimum, new(big.Int).Sub(new(big.Int).Set(price), big.NewInt(1))).Quo(minimum, price)
	minimum.Add(minimum, big.NewInt(1))
	if !minimum.IsUint64() || minimum.Uint64() >= values.CollateralRaw {
		return 0, budgetHold("pilot_release_minimum_collateral_unavailable")
	}
	allowance = min(allowance, values.CollateralRaw-minimum.Uint64())
	maxLTV := int64(limits.MaxLTVPct) * 100
	allowed := new(big.Int).Mul(new(big.Int).SetUint64(values.CollateralRaw), price)
	allowed.Quo(allowed, collateralScale).Mul(allowed, big.NewInt(maxLTV)).Quo(allowed, big.NewInt(10_000))
	// LendingMarket.globalAllowedBorrowValue is a whole-dollar u64 at 152.
	global := new(big.Int).Lsh(new(big.Int).SetUint64(limits.GlobalAllowedBorrowValue), 60)
	if allowed.Cmp(global) > 0 {
		allowed = global
	}
	room := new(big.Int).Sub(allowed, futureDebt)
	if room.Sign() <= 0 {
		return 0, budgetHold("pilot_release_protocol_allowance_unavailable")
	}
	// MaxLtv withdrawal: (allowed - adjusted debt) / collateral max LTV.
	room.Mul(room, big.NewInt(10_000))
	room.Mul(room, collateralScale)
	room.Quo(room, new(big.Int).Mul(big.NewInt(maxLTV), price))
	// One liquidity raw unit absorbs protocol fractional ratio rounding.
	room.Sub(room, big.NewInt(1))
	if !room.IsUint64() || room.Sign() <= 0 {
		return 0, budgetHold("pilot_release_protocol_allowance_unavailable")
	}
	return min(allowance, room.Uint64()), nil
}

const kaminoGlobalBorrowValueOffset = 152
const kaminoMinRemainingValueOffset = 3224
