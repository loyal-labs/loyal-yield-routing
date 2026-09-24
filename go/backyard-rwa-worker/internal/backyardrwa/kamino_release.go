package backyardrwa

import (
	"bytes"
	"context"
	"encoding/binary"
	"math/big"
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
func validateRepaymentReleaseRequest(ctx context.Context, rpc *RPCClient, request KaminoPrimeUSDCRequest, effects ExpectedEffects, slot int64) (KaminoReleaseBound, []ConfirmedAccount, error) {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return KaminoReleaseBound{}, nil, err
	}
	return manifest.validateRepaymentReleaseRequest(ctx, rpc, request, effects, slot)
}

// The manifest-aware form keeps every release-size, custody and effects check
// unchanged and only lets the candidate AUTO source path measure its request
// through the SAME reviewed manifest that produced it.
func (m RouteManifest) validateRepaymentReleaseRequest(ctx context.Context, rpc *RPCClient, request KaminoPrimeUSDCRequest, effects ExpectedEffects, slot int64) (KaminoReleaseBound, []ConfirmedAccount, error) {
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
	observed, accounts, err := observeKaminoPayoffWindowAccounts(ctx, rpc, route, slot, 5, payoffAdditional...)
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

// The manifest-aware form admits exactly one candidate lane: the reviewed AUTO
// binding's release, through the SAME checked risk arithmetic the installed
// pilot lanes use. Every public gate and the installed-lane behavior stay
// byte-identical; an absent or drifted binding errors instead of falling back.
func (m RouteManifest) pilotRepaymentLiquidityAllowance(accounts []ConfirmedAccount, route RuntimeRoute, position KaminoPosition, liquidationPct byte) (uint64, error) {
	if selectorLane(route.Lane) && route.Kamino.DebtMint == bridgeUSDC {
		return pilotRepaymentLiquidityAllowance(accounts, route, position, liquidationPct)
	}
	if route.Lane != autoAUTOPYUSD.Lane {
		return 0, budgetHold("pilot_release_lane_unreviewed")
	}
	if _, err := m.autoPolicyBinding(); err != nil {
		return 0, err
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
	if len(o) != kaminoObligationLength || len(c) != kaminoReserveLength || len(d) != kaminoReserveLength ||
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

func (b Phase3Budget) validatePilotReleaseAuthority(request any) error {
	if r, ok := request.(KaminoPrimeUSDCRequest); ok && r.PilotRepaymentRelease && b.Pilot == nil {
		return budgetHold("pilot_release_authority_required")
	}
	return nil
}

// An entry which relies on a future release cannot inherit that release after
// its risk inputs change. Compare the non-mutated inputs of the admitted
// simulation with one fresh batch before signing and sending. Drift requires
// a newly quoted complete plan, even when it might be economically favorable.
func validatePilotProjectedReleaseRisk(ctx context.Context, rpc *RPCClient, plan *phase3BridgeAdmission, slot int64) (int64, error) {
	if plan == nil || !plan.Snapshot.PilotActive || (plan.FundingRelease == nil && plan.BorrowRelease == nil && plan.RepaymentProjection == nil) {
		return slot, nil
	}
	projection := plan.DepositProjection
	if projection == nil {
		projection = plan.BorrowProjection
	}
	if projection == nil {
		projection = plan.LeverageProjection
	}
	if projection == nil {
		projection = plan.RepaymentProjection
	}
	// Funding/NAV continuation plans use actual release revalidation; only
	// entry simulations introduce the prospective post-entry position here.
	if projection == nil {
		return slot, nil
	}
	route, err := runtimeRoute(plan.Snapshot.RouteLane)
	if err != nil || rpc == nil {
		return 0, budgetHold("pilot_release_projection_unavailable")
	}
	if !selectorLane(route.Lane) {
		if route.Lane != autoAUTOPYUSD.Lane {
			return 0, budgetHold("pilot_release_projection_unavailable")
		}
		manifest, err := loadEmbeddedRouteManifest()
		if err != nil {
			return 0, err
		}
		if _, err = manifest.autoPolicyBinding(); err != nil {
			return 0, err
		}
	}
	if plan.Input == nil {
		return 0, budgetHold("pilot_release_projection_unavailable")
	}
	_, _, message, err := plan.Input.decode()
	if err != nil || sha256Bytes(message) != projection.MessageSHA256 {
		return 0, budgetHold("pilot_release_projection_identity_changed")
	}
	var addresses []string
	for _, a := range projection.Accounts {
		addresses = append(addresses, a.Address)
	}
	// The original entry simulation refreshed reserves. Re-simulate the same
	// unsigned message so prices have equivalent semantics; unrefreshed chain
	// prices can differ indefinitely even when the oracle has not moved.
	fresh, err := rpc.simulatePhase3EntryProjection(ctx, message, addresses, slot)
	if err != nil {
		return 0, err
	}
	if plan.RepaymentProjection != nil {
		request, effects, _, err := plan.Input.decode()
		r, ok := request.(KaminoPrimeUSDCRequest)
		if err != nil || !ok || fresh.Slot < projection.Slot || fresh.Slot > plan.ValidThroughSlot {
			return 0, budgetHold("partial_repayment_projection_expired")
		}
		bound, err := validatePartialRepaymentProjection(r, effects, plan.Snapshot, fresh)
		if err != nil {
			return 0, err
		}
		if plan.Payoff == nil || bound.UpperDebtRaw > plan.Payoff.UpperDebtRaw || bound.InterestBasis != plan.Payoff.InterestBasis || bound.MaximumRateBPS > plan.Payoff.MaximumRateBPS || bound.ChainUnix < plan.Payoff.ChainUnix || bound.ChainUnix > plan.Payoff.ChainUnix+kaminoPayoffWindowSeconds {
			return 0, budgetHold("partial_repayment_projection_payoff_changed")
		}
		if err = validateProjectedRiskSettings(*projection, fresh, route); err != nil {
			return 0, err
		}
		oldReserve, err := decodeKaminoReserve(accountAt(projection.Accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
		if err != nil {
			return 0, err
		}
		newReserve, err := decodeKaminoReserve(accountAt(fresh.Accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
		if err != nil {
			return 0, err
		}
		oldRaw, oldErr := oldReserve.redeemLiquidityRaw(uint64(plan.Snapshot.PositionCollateralRaw))
		newRaw, newErr := newReserve.redeemLiquidityRaw(uint64(plan.Snapshot.PositionCollateralRaw))
		// Even positive backing drift can enlarge the gross collateral return.
		// Reprice it instead of inheriting an envelope for a smaller amount.
		if oldErr != nil || newErr != nil || oldRaw != newRaw {
			return 0, budgetHold("partial_repayment_projection_backing_changed")
		}
		if plan.FundingRelease == nil && plan.BorrowRelease == nil {
			return fresh.Slot, nil
		}
	}
	if err = validatePilotReleaseProjection(plan, *projection, fresh, route); err != nil {
		return 0, err
	}
	return fresh.Slot, nil
}

func validatePilotReleaseProjection(plan *phase3BridgeAdmission, projection, fresh phase3KaminoProjection, route RuntimeRoute) error {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return err
	}
	if fresh.Slot > plan.ValidThroughSlot || fresh.Slot < projection.Slot {
		return budgetHold("pilot_release_projection_expired")
	}
	if err := validateProjectedRiskSettings(projection, fresh, route); err != nil {
		return err
	}
	oldReserve, err := decodeKaminoReserve(accountAt(projection.Accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return err
	}
	newReserve, err := decodeKaminoReserve(accountAt(fresh.Accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return err
	}
	// Compare normalized backing, not mutable raw pool totals. Normal interest
	// growth is allowed; a loss in existing receipt backing requires re-admission.
	left := new(big.Int).Mul(newReserve.totalLiquiditySF, new(big.Int).SetUint64(oldReserve.collateralMintSupply))
	right := new(big.Int).Mul(oldReserve.totalLiquiditySF, new(big.Int).SetUint64(newReserve.collateralMintSupply))
	if left.Cmp(right) < 0 {
		return budgetHold("pilot_release_projection_backing_reduced")
	}
	// The current entry has now been simulated, leaving six of the admitted
	// seven execution windows. Its complete-payoff bound must still fit.
	bound, err := manifest.decodeKaminoRepaymentReleaseForMode(fresh.Accounts, route, fresh.Slot, 6, true)
	if err != nil {
		return err
	}
	if plan.Payoff == nil || bound.Payoff.UpperDebtRaw > plan.Payoff.UpperDebtRaw || bound.Payoff.ChainUnix < plan.Payoff.ChainUnix || bound.Payoff.ChainUnix > plan.Payoff.ChainUnix+kaminoPayoffWindowSeconds {
		return budgetHold("pilot_release_projection_payoff_changed")
	}
	release := plan.FundingRelease
	if release == nil {
		release = plan.BorrowRelease
	}
	request, effects, _, err := release.decode()
	r, ok := request.(KaminoPrimeUSDCRequest)
	if err != nil || !ok || r.AmountRaw > bound.ReceiptRaw || len(effects.Accounts) != 2 {
		return budgetHold("pilot_release_projection_funding_changed")
	}
	liquidity, err := newReserve.redeemLiquidityRaw(r.AmountRaw)
	if err != nil || effects.Accounts[1].AfterRaw < effects.Accounts[1].BeforeRaw || liquidity < effects.Accounts[1].AfterRaw-effects.Accounts[1].BeforeRaw {
		return budgetHold("pilot_release_projection_funding_changed")
	}
	oldPosition, err := decodeKaminoObligation(accountAt(projection.Accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil {
		return err
	}
	newPosition, err := decodeKaminoObligation(accountAt(fresh.Accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil {
		return err
	}
	oldValue, err := oldReserve.redeemLiquidityRaw(oldPosition.collateralDepositedRaw)
	if err != nil {
		return err
	}
	newValue, err := newReserve.redeemLiquidityRaw(newPosition.collateralDepositedRaw)
	if err != nil || newValue < oldValue {
		return budgetHold("pilot_release_projection_collateral_reduced")
	}
	oldCash, newCash := accountAt(projection.Accounts, route.CollateralCustody), accountAt(fresh.Accounts, route.CollateralCustody)
	mint, _ := decodeBase58PublicKey(route.Kamino.CollateralMint)
	owner, _ := decodeBase58PublicKey(bridgeVault)
	oldBalance, oldErr := DecodeTokenCustody(oldCash.Owner, oldCash.Data, mint, owner)
	newBalance, newErr := DecodeTokenCustody(newCash.Owner, newCash.Data, mint, owner)
	if oldErr != nil || newErr != nil || newCash.Executable || newCash.Lamports == 0 || newBalance.Raw < oldBalance.Raw {
		return budgetHold("pilot_release_projection_funding_changed")
	}
	return nil
}

func validateProjectedRiskSettings(projection, fresh phase3KaminoProjection, route RuntimeRoute) error {
	for _, field := range []struct {
		address    string
		start, end int
	}{
		{route.Kamino.Market, kaminoMarketEmergencyModeOffset, kaminoMarketEmergencyModeOffset + 1},
		{route.Kamino.Market, kaminoGlobalBorrowValueOffset, kaminoGlobalBorrowValueOffset + 8},
		{route.Kamino.Market, kaminoMinRemainingValueOffset, kaminoMinRemainingValueOffset + 16},
		{route.Kamino.CollateralReserve, kaminoLoanToValueOffset, kaminoLoanToValueOffset + 2},
		{route.Kamino.CollateralReserve, 248, 264},
		{route.Kamino.DebtReserve, 248, 264},
		{route.Kamino.DebtReserve, kaminoBorrowFactorOffset, kaminoBorrowFactorOffset + 8},
		{route.Kamino.Obligation, kaminoObligationElevationGroupOffset, kaminoObligationElevationGroupOffset + 1},
	} {
		before, after := accountAt(projection.Accounts, field.address), accountAt(fresh.Accounts, field.address)
		if before.Owner != route.Kamino.Program || after.Owner != before.Owner || after.Executable || after.Lamports == 0 ||
			len(before.Data) != len(after.Data) || len(before.Data) < field.end || !bytes.Equal(before.Data[field.start:field.end], after.Data[field.start:field.end]) {
			return budgetHold("pilot_release_projection_risk_changed")
		}
	}
	return nil
}

// observeRawRepaymentRelease sizes a repayment release on a raw payoff-window
// capture: build and send re-check the release on raw reserves, whose older
// rate allows a slightly smaller release than the refreshed-reserve
// simulation the route snapshot is priced on (live 2026-09-24: every sized
// release held with repayment_release_exceeds_safe_size). Sizing one window
// longer than the five-step re-check leaves headroom for the slots between
// build and send.
func (m RouteManifest) observeRawRepaymentRelease(ctx context.Context, rpc *RPCClient, route RuntimeRoute, slot int64, pilot bool) (KaminoReleaseBound, []ConfirmedAccount, error) {
	var additional []string
	if pilot && route.Lane == autoAUTOPYUSD.Lane {
		additional = append(additional, route.Kamino.Market)
	}
	observed, accounts, err := observeKaminoPayoffWindowAccounts(ctx, rpc, route, slot, 6, additional...)
	if err != nil {
		return KaminoReleaseBound{}, nil, err
	}
	bound, err := m.decodeKaminoRepaymentReleaseForMode(accounts, route, observed.ObservedSlot, 6, pilot)
	return bound, accounts, err
}
