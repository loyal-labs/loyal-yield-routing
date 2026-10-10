package backyard

import (
	"bytes"
	"context"
	"encoding/binary"
	"math/big"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

type KaminoReleaseBound struct {
	Payoff              KaminoPayoffBound `json:"payoff"`
	ReceiptRaw          uint64            `json:"receiptRaw"`
	LiquidityRaw        uint64            `json:"liquidityRaw"`
	RemainingReceiptRaw uint64            `json:"remainingReceiptRaw"`
}

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

// Exact cost-only pool transition: burn receipt supply and debit released
// liquidity. Tested against the deployed program's actual post-release reserve.
func projectKaminoReleaseReserve(reserve decodedKaminoReserve, receipts, liquidity uint64) (decodedKaminoReserve, error) {
	if receipts == 0 || receipts >= reserve.collateralMintSupply || liquidity == 0 {
		return reserve, budgetHold("invalid_release_reserve_projection")
	}
	debit := new(big.Int).Lsh(new(big.Int).SetUint64(liquidity), 60)
	if reserve.totalLiquiditySF == nil || reserve.totalLiquiditySF.Cmp(debit) <= 0 {
		return reserve, budgetHold("invalid_release_reserve_projection")
	}
	reserve.totalLiquiditySF = new(big.Int).Sub(reserve.totalLiquiditySF, debit)
	reserve.collateralMintSupply -= receipts
	return reserve, nil
}

// Keep the existing unwind LTV, but size against debt through all five steps
// before payoff. Convert the conservative liquidity allowance back to receipts
// using the reserve's unrounded exchange rate, not a rounded position ratio.
func decodeKaminoRepaymentRelease(accounts []ConfirmedAccount, route RuntimeRoute, slot int64) (KaminoReleaseBound, error) {
	return decodeKaminoRepaymentReleaseWindow(accounts, route, slot, 5)
}

func decodeKaminoRepaymentReleaseWindow(accounts []ConfirmedAccount, route RuntimeRoute, slot, steps int64) (KaminoReleaseBound, error) {
	return decodeKaminoRepaymentReleaseForMode(accounts, route, slot, steps, false)
}

func decodeKaminoRepaymentReleaseForMode(accounts []ConfirmedAccount, route RuntimeRoute, slot, steps int64, pilot bool) (KaminoReleaseBound, error) {
	return decodeKaminoRepaymentReleaseWithAllowance(accounts, route, slot, steps, pilot, pilotRepaymentLiquidityAllowance)
}

// The manifest-aware internal form serves the candidate AUTO source path only:
// installed lanes resolve through the same public function as always, and the
// reviewed AUTO pilot release reuses the identical body with the manifest-aware
// allowance resolver. No gate, bound or receipt conversion changes.
func (m RouteManifest) decodeKaminoRepaymentReleaseForMode(accounts []ConfirmedAccount, route RuntimeRoute, slot, steps int64, pilot bool) (KaminoReleaseBound, error) {
	return decodeKaminoRepaymentReleaseWithAllowance(accounts, route, slot, steps, pilot, m.pilotRepaymentLiquidityAllowance)
}

func decodeKaminoRepaymentReleaseWithAllowance(accounts []ConfirmedAccount, route RuntimeRoute, slot, steps int64, pilot bool,
	allowanceFor func([]ConfirmedAccount, RuntimeRoute, KaminoPosition, byte) (uint64, error)) (KaminoReleaseBound, error) {
	var result KaminoReleaseBound
	bound, err := decodeKaminoPayoffWindow(accounts, route, slot, steps)
	if err != nil {
		return result, err
	}
	obligation, err := decodeKaminoObligation(accountAt(accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil {
		return result, err
	}
	collateral, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return result, err
	}
	debt, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.DebtReserve), route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		return result, err
	}
	if err := validateKaminoRefresh(obligation, collateral, debt); err != nil {
		return result, err
	}
	total, err := collateral.redeemLiquidityRaw(obligation.collateralDepositedRaw)
	if err != nil {
		return result, err
	}
	position := KaminoPosition{CollateralDepositedRaw: obligation.collateralDepositedRaw, RedeemablePrimeRaw: total, DebtRaw: bound.UpperDebtRaw,
		CollateralDecimals: collateral.mintDecimals, DebtDecimals: debt.mintDecimals, CollateralPriceSF: collateral.marketPriceSF, DebtPriceSF: debt.marketPriceSF}
	_, allowance, err := withdrawExcessForRepayment(position)
	if pilot {
		allowance, err = allowanceFor(accounts, route, position, collateral.liquidationThresholdPct)
	}
	if err != nil {
		return result, budgetHold("no_safe_repayment_collateral_release")
	}
	denominator := new(big.Int).Lsh(new(big.Int).SetUint64(collateral.collateralMintSupply), 60)
	receipt := new(big.Int).Mul(new(big.Int).SetUint64(allowance), denominator)
	receipt.Quo(receipt, collateral.totalLiquiditySF)
	if !receipt.IsUint64() || receipt.Sign() <= 0 || receipt.Uint64() >= obligation.collateralDepositedRaw {
		return result, budgetHold("invalid_repayment_release_receipts")
	}
	liquidity, err := collateral.redeemLiquidityRaw(receipt.Uint64())
	if err != nil || liquidity == 0 || liquidity > allowance {
		return result, budgetHold("invalid_repayment_release_liquidity")
	}
	return KaminoReleaseBound{Payoff: bound, ReceiptRaw: receipt.Uint64(), LiquidityRaw: liquidity, RemainingReceiptRaw: obligation.collateralDepositedRaw - receipt.Uint64()}, nil
}

// Recheck the actual persisted release at build and final send. A smaller
// already-admitted amount may remain safe, but stale effects cannot survive a
// changed exchange rate/custody or an interest/price move beyond the safe size.
func validateRepaymentReleaseRequest(ctx context.Context, view *View, request KaminoPrimeUSDCRequest, effects ExpectedEffects, slot int64) (KaminoReleaseBound, []ConfirmedAccount, error) {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return KaminoReleaseBound{}, nil, err
	}
	return manifest.validateRepaymentReleaseRequest(ctx, view, request, effects, slot)
}

// The manifest-aware form keeps every release-size, custody and effects check
// unchanged and only lets the candidate AUTO source path measure its request
// through the SAME reviewed manifest that produced it.
func (m RouteManifest) validateRepaymentReleaseRequest(ctx context.Context, view *View, request KaminoPrimeUSDCRequest, effects ExpectedEffects, slot int64) (KaminoReleaseBound, []ConfirmedAccount, error) {
	var result KaminoReleaseBound
	if !request.RepaymentRelease || request.FullPayoff {
		return result, nil, budgetHold("invalid_repayment_release_intent")
	}
	if _, err := m.measureExecutableDebit(request, effects); err != nil {
		return result, nil, err
	}
	route, err := runtimeRoute(request.RouteLane)
	if err != nil {
		return result, nil, err
	}
	// The pilot risk model reads the lending market, which the payoff window
	// captures only for installed selector lanes; the reviewed candidate lane
	// rides the same window request so the recheck keeps one coherent slot.
	payoffAdditional := []string(nil)
	if request.PilotRepaymentRelease && route.Lane == autoAUTOPYUSD.Lane {
		payoffAdditional = append(payoffAdditional, route.Kamino.Market)
	}
	observed, accounts, err := observeKaminoPayoffWindowAccounts(ctx, view, route, slot, 5, payoffAdditional...)
	if err != nil {
		return result, nil, err
	}
	result, err = m.decodeKaminoRepaymentReleaseForMode(accounts, route, observed.ObservedSlot, 5, request.PilotRepaymentRelease)
	if err != nil {
		return result, nil, err
	}
	mint, _ := decodeBase58PublicKey(route.Kamino.DebtMint)
	authority, _ := decodeBase58PublicKey(bridgeVault)
	debtAccount := accountAt(accounts, route.DebtCustody)
	debtCustody, err := DecodeTokenCustody(debtAccount.Owner, debtAccount.Data, mint, authority)
	if err != nil || debtAccount.Executable || debtAccount.Lamports == 0 || debtCustody.Raw != request.ReleaseDebtIdleRaw {
		return result, nil, budgetHold("repayment_release_debt_cash_changed")
	}
	if request.AmountRaw == 0 || request.AmountRaw > result.ReceiptRaw {
		return result, nil, budgetHold("repayment_release_exceeds_safe_size")
	}
	reserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return result, nil, err
	}
	amount, err := reserve.redeemLiquidityRaw(request.AmountRaw)
	if err != nil {
		return result, nil, err
	}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
	want, err := exactKaminoTokenEffects(accounts, source, destination, amount)
	if err != nil {
		return result, nil, err
	}
	actual, _ := jsonMarshalExpectedEffects(effects)
	expected, _ := jsonMarshalExpectedEffects(want)
	if !bytes.Equal(actual, expected) {
		return result, nil, budgetHold("repayment_release_effects_changed")
	}
	return result, accounts, nil
}

// A single 1.5x pass cannot fund its full payoff at the historical 45% release
// LTV. Pilot release stays at most 55%, with five percentage points below both
// the protocol max LTV and the unchanged worker hard stop. It also respects
// the market cap using current reserve prices, never cached obligation values.
// Inputs have already passed envelope, topology, refresh and payoff validation.
func pilotRepaymentLiquidityAllowance(accounts []ConfirmedAccount, route RuntimeRoute, position KaminoPosition, liquidationPct byte) (uint64, error) {
	if !selectorLane(route.Lane) || route.Kamino.DebtMint != bridgeUSDC {
		return 0, budgetHold("pilot_release_lane_unreviewed")
	}
	return pilotRepaymentLiquidityAllowanceChecked(accounts, route, position, liquidationPct)
}

// The manifest-aware form admits exactly one more lane: the AUTO lane's
// release, through the SAME checked risk arithmetic the installed pilot lanes
// use. Every public gate and the installed-lane behavior stay byte-identical.
func (m RouteManifest) pilotRepaymentLiquidityAllowance(accounts []ConfirmedAccount, route RuntimeRoute, position KaminoPosition, liquidationPct byte) (uint64, error) {
	if selectorLane(route.Lane) && route.Kamino.DebtMint == bridgeUSDC {
		return pilotRepaymentLiquidityAllowance(accounts, route, position, liquidationPct)
	}
	if route.Lane != autoAUTOPYUSD.Lane {
		return 0, budgetHold("pilot_release_lane_unreviewed")
	}
	return pilotRepaymentLiquidityAllowanceChecked(accounts, route, position, liquidationPct)
}

// pilotRepaymentLiquidityAllowanceChecked is the lane-independent core shared
// by both forms above: market risk model, LTV ceiling, and the existing
// rounded and ForValues allowance arithmetic with its receipt bounds.
func pilotRepaymentLiquidityAllowanceChecked(accounts []ConfirmedAccount, route RuntimeRoute, position KaminoPosition, liquidationPct byte) (uint64, error) {
	market := accountAt(accounts, route.Kamino.Market)
	if emergency, err := decodeKaminoMarketEmergency(market, route.Kamino); err != nil {
		return 0, err
	} else if emergency {
		return 0, budgetHold("pilot_release_market_emergency")
	}
	o := accountAt(accounts, route.Kamino.Obligation).Data
	c := accountAt(accounts, route.Kamino.CollateralReserve).Data
	d := accountAt(accounts, route.Kamino.DebtReserve).Data
	if len(o) != kamino.ObligationSize || len(c) != kamino.ReserveSize || len(d) != kamino.ReserveSize ||
		o[kaminoObligationElevationGroupOffset] != 0 || max(binary.LittleEndian.Uint64(d[kaminoBorrowFactorOffset:]), 100) != 100 {
		return 0, budgetHold("pilot_release_risk_model_changed")
	}
	limits := kaminoPilotReleaseLimits{MaxLTVPct: c[kaminoLoanToValueOffset], LiquidationPct: liquidationPct,
		GlobalAllowedBorrowValue: binary.LittleEndian.Uint64(market.Data[kaminoGlobalBorrowValueOffset:])}
	copy(limits.MinimumRemainingValueSF[:], market.Data[kaminoMinRemainingValueOffset:kaminoMinRemainingValueOffset+16])
	ceiling, err := limits.ceilingBPS()
	if err != nil {
		return 0, err
	}
	// Preserve the runtime's existing receipt-ratio floors. Scalar forecasts
	// have no receipts; actual execution must still retain this tighter bound.
	_, roundedAllowance, err := withdrawExcessAtLTV(position, ceiling)
	if err != nil {
		return 0, err
	}
	allowance, err := pilotRepaymentLiquidityAllowanceForValues(releaseValuesForPosition(position), limits)
	return min(roundedAllowance, allowance), err
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

// observeRawRepaymentRelease sizes a repayment release on a raw payoff-window
// capture: build and send re-check the release on raw reserves, whose older
// rate allows a slightly smaller release than the refreshed-reserve
// simulation the route snapshot is priced on (live 2026-09-24: every sized
// release held with repayment_release_exceeds_safe_size). Sizing one window
// longer than the five-step re-check leaves headroom for the slots between
// build and send.
func (m RouteManifest) observeRawRepaymentRelease(ctx context.Context, view *View, route RuntimeRoute, slot int64) (KaminoReleaseBound, []ConfirmedAccount, error) {
	var additional []string
	if route.Lane == autoAUTOPYUSD.Lane {
		additional = append(additional, route.Kamino.Market)
	}
	observed, accounts, err := observeKaminoPayoffWindowAccounts(ctx, view, route, slot, 6, additional...)
	if err != nil {
		return KaminoReleaseBound{}, nil, err
	}
	bound, err := m.decodeKaminoRepaymentReleaseForMode(accounts, route, observed.ObservedSlot, 6, true)
	return bound, accounts, err
}
