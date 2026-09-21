package backyardrwa

import (
	"context"
	"encoding/binary"
	"math"
	"math/big"
)

// Debt-sensitive interval, enumerated from the retained destination recipe:
// borrow, NAV, leverage swap, NAV, redeposit, NAV, funding withdrawal, NAV,
// funding swap, repay — exactly ten messages from the borrow through the
// maximum repayment. The eleventh window covers the borrow's own observation
// lag after the reserve refresh. Return-only steps after the repay add no
// debt accrual and are deliberately excluded from this horizon.
const selectorPayoffWindows int64 = 11

// Receipt-rounding horizon, UNCHANGED from the original single-window proof:
// the full eighteen-message structural recipe compounded the per-receipt
// rounding bound, and Maple's rounding protection keeps exactly that horizon.
// Refining the AUTO ledger's own intervals below never narrows it.
const selectorReceiptWindows int64 = 18

// Collateral-sensitive interval, enumerated from the same recipe: first
// deposit, NAV, borrow, NAV, leverage swap, NAV, redeposit, NAV, funding
// withdrawal, NAV, funding swap, repay, NAV, post-repay withdrawal — exactly
// fourteen messages from the first deposit through the LAST redemption. Two
// spare windows cover the deposits' own observation lags. Conversion and
// residue steps after the final redemption move no receipt backing and are
// deliberately excluded. Used only for the AUTO owned-receipt budget ceiling;
// the Maple rounding horizon above stays at its original eighteen windows.
const selectorCollateralWindows int64 = 16

// Structural bound on the complete retained recipe: the initializer
// (optional), the enumerated entry tail, and the full payoff return through
// the idle restore. The pricing consumer refuses longer recipes instead of
// silently pricing beyond the bounded windows above.
const selectorFullRecipeWindows int64 = 25

func selectorProspectiveUpper(accounts []ConfirmedAccount, route RuntimeRoute, slot int64, address, mint string, amountSF *big.Int, windows int64) (uint64, error) {
	a := accountAt(accounts, address)
	reserve, err := decodeKaminoReserve(a, mint, route.Kamino)
	if err != nil {
		return 0, err
	}
	clock := accountAt(accounts, budgetClockAddress)
	if windows < 1 || windows > selectorFullRecipeWindows || slot <= 0 || slot > math.MaxInt64-windows*budgetMaxObservationLagSlots-1 || clock.Owner != "Sysvar1111111111111111111111111111111111111" || clock.Executable || len(clock.Data) != 40 {
		return 0, budgetHold("invalid_payoff_clock")
	}
	clockSlot := binary.LittleEndian.Uint64(clock.Data[:8])
	now := int64(binary.LittleEndian.Uint64(clock.Data[32:40]))
	if clockSlot < uint64(slot) || clockSlot > uint64(slot)+1 || reserve.refreshedSlot <= 0 || reserve.refreshedSlot > int64(clockSlot) || now <= 0 || now > math.MaxInt64-windows*kaminoPayoffWindowSeconds {
		return 0, budgetHold("invalid_payoff_clock")
	}
	config := a.Data[kaminoReserveConfigOffset:]
	if config[9] > 1 || config[7] != 0 || !allZero(config[920:936]) {
		return 0, budgetHold("unsupported_payoff_interest_terms")
	}
	rate, err := kaminoMaximumBorrowRate(config)
	if err != nil {
		return 0, err
	}
	elapsed, units := int64(clockSlot)+windows*budgetMaxObservationLagSlots-reserve.refreshedSlot, uint64(63_072_000)
	if config[9] == 1 {
		updated := int64(binary.LittleEndian.Uint32(a.Data[28:32]))
		if updated <= 0 || updated > now {
			return 0, budgetHold("invalid_payoff_refresh_timestamp")
		}
		elapsed, units = now+windows*kaminoPayoffWindowSeconds-updated, 31_536_000
	}
	return upperKaminoCompoundedDebtSF(amountSF, rate, uint64(elapsed), units)
}

// selectorCollateralPoolUpper compounds the collateral pool's liquidity over
// `windows` bounded windows — the SAME bounded compounding machinery the debt
// upper uses, applied to the collateral pool plus the two deposits' backing
// units. Units are RAW collateral throughout: the input adds two raw backing
// units to the SF pool and the compounded result is floored back to raw by
// the payoff helper, exactly like the debt upper. It is the shared ceiling
// behind both the per-receipt rounding bound (original eighteen-window
// horizon) and the guaranteed owned-receipt budget (enumerated collateral
// interval).
func selectorCollateralPoolUpper(accounts []ConfirmedAccount, route RuntimeRoute, slot int64, windows int64) (uint64, error) {
	reserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil || reserve.collateralMintSupply == 0 {
		return 0, budgetHold("selector_destination_receipt_bound_unavailable")
	}
	pool := new(big.Int).Add(reserve.totalLiquiditySF, new(big.Int).Lsh(big.NewInt(2), 60))
	return selectorProspectiveUpper(accounts, route, slot, route.Kamino.CollateralReserve, route.Kamino.CollateralMint, pool, windows)
}

// Each of the two deposits mints floor(request/rate) receipts and transfers
// ceil(receipts*rate) liquidity, adding <1 raw unit of rounding to backing.
// Add both units before compounding, keep only the existing receipt supply,
// and round up to bound one receipt throughout the prospective exit window.
// This is the ORIGINAL proof's bound and keeps its original eighteen-window
// compounding horizon for every lane.
func selectorReceiptRounding(accounts []ConfirmedAccount, route RuntimeRoute, slot int64) (uint64, error) {
	reserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil || reserve.collateralMintSupply == 0 {
		return 0, budgetHold("selector_destination_receipt_bound_unavailable")
	}
	upper, err := selectorCollateralPoolUpper(accounts, route, slot, selectorReceiptWindows)
	if err != nil {
		return 0, err
	}
	unit := upper / reserve.collateralMintSupply
	if upper%reserve.collateralMintSupply != 0 {
		unit++
	}
	if unit == 0 || unit > math.MaxUint64/2 {
		return 0, budgetHold("selector_destination_receipt_bound_unavailable")
	}
	return unit, nil
}

// selectorDestinationPayoff is the guaranteed-proceeds exit bound for a
// destination position. Funding is the debt-covering leg; Legs are every
// payoff swap leg in call order, retained as recipe evidence by the caller.
// The withdrawal ledger is receipt-exact: wires are receipt counts bounded by
// the GUARANTEED owned receipt budget, effects are the floored liquidity
// those receipts redeem, and total redeemed liquidity can never exceed total
// deposited. Actual future receipt counts are NOT claimed as known: they can
// only exceed the guaranteed budget, and the unproven sliver is named for a
// refreshed continuation pass, never wired now.
type selectorDestinationPayoff struct {
	DebtUpperRaw    uint64
	Funding         JupiterExecutionEvidence
	Legs            []JupiterExecutionEvidence
	ResidueInputRaw uint64 // guaranteed PYUSD residue input = funding minimum - debt upper
	ReturnAmountRaw uint64 // guaranteed post-repay redemption: every remaining guaranteed receipt
	// Receipt-exact redemption ledger shared with the retained step templates.
	OwnedReceiptsRaw    uint64 // GUARANTEED receipts the two deposits mint at the compounded rate upper
	ResidualReceiptsRaw uint64 // receipts beyond the guarantee (rate at deposit time may be lower than the compounded upper) — refreshed continuation, never wired
	FundingReceiptsRaw  uint64 // funding withdrawal wire: receipts ceiled to guarantee the sized input
	ReturnReceiptsRaw   uint64 // post-repay withdrawal wire: every remaining guaranteed receipt
	FundingLiquidityRaw uint64 // guaranteed liquidity the funding wire redeems
	CustodyLeftoverRaw  uint64 // guaranteed collateral custody left after the funding swap (funding redemption - sized)
}

// payoffDepositReceiptsAt floors a deposit's receipt mint at the liquidity
// numerator `liquiditySF` (a pool ceiling or the current pool). Both supply
// factors participate: receipts = floor(deposit * mintSupply * 2^60 /
// liquiditySF). Rate and decimals are non-unit safe — only raw receipt and
// liquidity counts multiply.
func payoffDepositReceiptsAt(liquiditySF *big.Int, mintSupply, deposit uint64) (uint64, error) {
	if deposit == 0 || liquiditySF == nil || liquiditySF.Sign() <= 0 || mintSupply == 0 {
		return 0, budgetHold("selector_destination_receipt_bound_unavailable")
	}
	numerator := new(big.Int).Mul(new(big.Int).SetUint64(deposit), new(big.Int).Lsh(new(big.Int).SetUint64(mintSupply), 60))
	receipts := new(big.Int).Quo(numerator, liquiditySF)
	if !receipts.IsUint64() {
		return 0, budgetHold("selector_destination_receipt_bound_unavailable")
	}
	return receipts.Uint64(), nil
}

// payoffOwnedReceiptsLower bounds from BELOW the receipts a future deposit
// will mint: minting happens at the then-current collateral rate, which the
// bounded compounding ceiling upper-bounds, so floor(deposit at the ceiling)
// receipts is guaranteed no matter when the deposit lands.
func payoffOwnedReceiptsLower(poolUpperSF *big.Int, mintSupply, deposit uint64) (uint64, error) {
	return payoffDepositReceiptsAt(poolUpperSF, mintSupply, deposit)
}

// payoffReceiptsForLiquidity ceils the receipt count whose redemption at ANY
// future rate guarantees the liquidity target: the collateral rate only rises,
// so floor(wire * rateFuture) >= floor(wire * rateNow) >= the target.
func payoffReceiptsForLiquidity(totalLiquiditySF *big.Int, mintSupply, liquidity uint64) (uint64, error) {
	if liquidity == 0 || totalLiquiditySF == nil || totalLiquiditySF.Sign() <= 0 || mintSupply == 0 {
		return 0, budgetHold("selector_destination_receipt_bound_unavailable")
	}
	numerator := new(big.Int).Mul(new(big.Int).SetUint64(liquidity), new(big.Int).Lsh(new(big.Int).SetUint64(mintSupply), 60))
	receipts := new(big.Int).Add(numerator, totalLiquiditySF)
	receipts.Sub(receipts, big.NewInt(1))
	receipts.Quo(receipts, totalLiquiditySF)
	if !receipts.IsUint64() {
		return 0, budgetHold("selector_destination_receipt_bound_unavailable")
	}
	return receipts.Uint64(), nil
}

// payoffLiquidityForReceipts floors the liquidity a receipt count redeems at
// the observed rate. Redemption at any later rate floors to at least this.
func payoffLiquidityForReceipts(totalLiquiditySF *big.Int, mintSupply, receipts uint64) (uint64, error) {
	if receipts == 0 {
		return 0, nil
	}
	if totalLiquiditySF == nil || totalLiquiditySF.Sign() <= 0 || mintSupply == 0 {
		return 0, budgetHold("selector_destination_receipt_bound_unavailable")
	}
	liquidity := new(big.Int).Mul(new(big.Int).SetUint64(receipts), totalLiquiditySF)
	liquidity.Quo(liquidity, new(big.Int).Lsh(new(big.Int).SetUint64(mintSupply), 60))
	if !liquidity.IsUint64() {
		return 0, budgetHold("selector_destination_receipt_bound_unavailable")
	}
	return liquidity.Uint64(), nil
}

// selectorPayoffFundingSize sizes the repay-funding leg from the probe quote
// taken at the full released collateral: the minimal collateral input whose
// probe-rate minimum output reaches the compounded debt upper. The second
// funding quote's own minimum — not this sizing — carries the guarantee. The
// quotient is always ceiled on a positive remainder (a positive numerator
// below the denominator yields one, never zero) and a zero result is
// rejected. Raw debt and raw collateral units are never compared directly;
// the product uses big.Int because both operands are unbounded u64.
func selectorPayoffFundingSize(debtUpper, released, probeMinimumOutput uint64) (uint64, error) {
	if debtUpper == 0 || released == 0 || probeMinimumOutput == 0 {
		return 0, budgetHold("selector_destination_payoff_quote_insufficient")
	}
	numerator := new(big.Int).Mul(new(big.Int).SetUint64(debtUpper), new(big.Int).SetUint64(released))
	denominator := new(big.Int).SetUint64(probeMinimumOutput)
	sized := new(big.Int).Quo(numerator, denominator)
	if new(big.Int).Mod(numerator, denominator).Sign() != 0 {
		sized.Add(sized, big.NewInt(1))
	}
	if sized.Sign() == 0 || !sized.IsUint64() || sized.Uint64() > released {
		return 0, budgetHold("selector_destination_payoff_quote_insufficient")
	}
	return sized.Uint64(), nil
}

// The payoff proof prices the full repay-funding and residual-return recipe in
// both mints, never a single collateral-to-USDC leg compared against raw debt
// units. Every balance below is an explicit future scalar passed to the quote
// producer — a cost-only forecast, never a simulated account image. A quote,
// build or minimum-output failure blocks the non-USDC lane while USDC lanes
// keep their existing single-leg proof.
//
// The withdrawal ledger is receipt-exact and conserved without claiming the
// future is known. The two deposits will mint at least OwnedReceiptsRaw
// receipts (minting uses the then-current rate, upper-bounded by the bounded
// compounding ceiling); the funding wire is the receipt count that guarantees
// `sized` and must fit inside that guaranteed budget; the post-repay wire
// redeems every remaining GUARANTEED receipt. Each effect amount is the
// floored redemption of its wire at the observed rate — a guaranteed lower
// bound — so total redeemed collateral stays at or below the deposited
// liquidity, the funding swap sells from guaranteed custody only, and no
// rounding balance is manufactured. Receipts beyond the guarantee are
// ResidualReceiptsRaw: an upper uncertainty bound, unproven now, left for a
// refreshed continuation pass instead of being wired on today's evidence.
func selectorDestinationExit(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, route RuntimeRoute, accounts []ConfirmedAccount, position KaminoPosition, slot, observationFloor int64, entryDeposit, redepositDeposit, initial, redeposit, borrow, fee, rounding uint64) (selectorDestinationPayoff, error) {
	var empty selectorDestinationPayoff
	if initial <= 1 || redeposit <= 1 || initial-1 > math.MaxUint64-(redeposit-1) || borrow > math.MaxUint64-fee {
		return empty, budgetHold("selector_destination_exit_amount_invalid")
	}
	debt, err := selectorProspectiveUpper(accounts, route, slot, route.Kamino.DebtReserve, route.Kamino.DebtMint, new(big.Int).Lsh(new(big.Int).SetUint64(borrow+fee), 60), selectorPayoffWindows)
	if err != nil {
		return empty, err
	}
	market := accountAt(accounts, route.Kamino.Market).Data
	collateral := accountAt(accounts, route.Kamino.CollateralReserve).Data
	limits := kaminoPilotReleaseLimits{MaxLTVPct: collateral[kaminoLoanToValueOffset], LiquidationPct: collateral[kaminoReserveConfigOffset+17], GlobalAllowedBorrowValue: binary.LittleEndian.Uint64(market[kaminoGlobalBorrowValueOffset:])}
	copy(limits.MinimumRemainingValueSF[:], market[kaminoMinRemainingValueOffset:kaminoMinRemainingValueOffset+16])
	values := kaminoReleaseValues{CollateralRaw: initial - 1 + redeposit - 1, DebtRaw: debt, CollateralDecimals: position.CollateralDecimals, DebtDecimals: position.DebtDecimals, CollateralPriceSF: position.CollateralPriceSF, DebtPriceSF: position.DebtPriceSF}
	allowance, err := pilotRepaymentLiquidityAllowanceForValues(values, limits)
	if err != nil {
		return empty, err
	}
	if rounding == 0 || rounding > math.MaxUint64/2 || allowance <= 2*rounding || allowance > math.MaxInt64 {
		return empty, budgetHold("selector_destination_exit_rounding_unavailable")
	}
	released := allowance - 2*rounding
	if route.Kamino.DebtMint == bridgeUSDC {
		quote, err := prepareJupiterQuoteEvidence(ctx, rpc, client, m, Decision{Action: SwapCollateralToStableStep, AmountRaw: int64(released), StrategyKey: route.Lane}, released, 0, observationFloor)
		if err != nil {
			return empty, err
		}
		if quote.Request.MinimumOutputRaw < debt {
			return empty, budgetHold("selector_destination_payoff_quote_insufficient")
		}
		return selectorDestinationPayoff{DebtUpperRaw: debt, Funding: quote}, nil
	}
	// The guaranteed owned receipt budget: what the two future deposits mint
	// even at the compounded rate ceiling. Withdrawal wires are checked
	// against this budget — never against the whole reserve supply.
	reserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return empty, err
	}
	poolUpperRaw, err := selectorCollateralPoolUpper(accounts, route, slot, selectorCollateralWindows)
	if err != nil {
		return empty, err
	}
	// Units: the compounded ceiling is a RAW collateral amount; every receipt
	// conversion divides by an SF-scaled numerator. Lift it explicitly so no
	// helper ever reads raw as SF (a 2^60 receipt inflation).
	poolUpperSF := new(big.Int).Lsh(new(big.Int).SetUint64(poolUpperRaw), 60)
	entryReceipts, err := payoffOwnedReceiptsLower(poolUpperSF, reserve.collateralMintSupply, entryDeposit)
	if err != nil {
		return empty, err
	}
	redepositReceipts, err := payoffOwnedReceiptsLower(poolUpperSF, reserve.collateralMintSupply, redepositDeposit)
	if err != nil {
		return empty, err
	}
	ownedReceipts, err := budgetSumU64(entryReceipts, redepositReceipts)
	if err != nil {
		return empty, err
	}
	// Funding is sized from a probe quote at the full release, then requoted
	// at the sized input: the guarantee is the second quote's own minimum
	// output, and the guaranteed residue is that minimum minus the maximum
	// repayment.
	probe, err := prepareJupiterQuoteEvidence(ctx, rpc, client, m, Decision{Action: SwapCollateralToDebtStep, AmountRaw: int64(released), StrategyKey: route.Lane}, released, 0, observationFloor)
	if err != nil {
		return empty, err
	}
	sized, err := selectorPayoffFundingSize(debt, released, probe.Request.MinimumOutputRaw)
	if err != nil {
		return empty, err
	}
	// The funding withdrawal wire is the receipt count whose redemption
	// guarantees `sized`; it must fit inside the guaranteed receipt budget,
	// and its guaranteed floored redemption must stay within the LTV release.
	fundingReceipts, err := payoffReceiptsForLiquidity(reserve.totalLiquiditySF, reserve.collateralMintSupply, sized)
	if err != nil {
		return empty, err
	}
	if fundingReceipts > ownedReceipts {
		return empty, budgetHold("selector_destination_payoff_quote_insufficient")
	}
	fundingLiquidity, err := payoffLiquidityForReceipts(reserve.totalLiquiditySF, reserve.collateralMintSupply, fundingReceipts)
	if err != nil {
		return empty, err
	}
	if fundingLiquidity < sized || fundingLiquidity > released {
		return empty, budgetHold("selector_destination_payoff_quote_insufficient")
	}
	// The wire's ACTUAL redemption happens later at a higher rate: the
	// current-rate floored output above is only the guaranteed LOWER bound
	// and cannot bound the withdrawal. Bound the future redemption from
	// ABOVE at the compounded pool ceiling and require that upper — never
	// the lower output — to fit inside the LTV release.
	fundingRedeemUpper, err := payoffLiquidityForReceipts(poolUpperSF, reserve.collateralMintSupply, fundingReceipts)
	if err != nil {
		return empty, err
	}
	if fundingRedeemUpper > released {
		return empty, budgetHold("selector_destination_payoff_release_upper_exceeded")
	}
	if fundingLiquidity > math.MaxInt64 {
		return empty, budgetHold("selector_destination_payoff_amount_overflow")
	}
	funding, err := prepareJupiterQuoteEvidence(ctx, rpc, client, m, Decision{Action: SwapCollateralToDebtStep, AmountRaw: int64(sized), StrategyKey: route.Lane}, fundingLiquidity, 0, observationFloor)
	if err != nil {
		return empty, err
	}
	if funding.Request.MinimumOutputRaw < debt {
		return empty, budgetHold("selector_destination_payoff_quote_insufficient")
	}
	payoff := selectorDestinationPayoff{
		DebtUpperRaw: debt, Funding: funding, Legs: []JupiterExecutionEvidence{funding},
		OwnedReceiptsRaw: ownedReceipts, FundingReceiptsRaw: fundingReceipts,
		FundingLiquidityRaw: fundingLiquidity, CustodyLeftoverRaw: fundingLiquidity - sized,
	}
	// The unproven sliver: the deposits could mint up to their current-rate
	// counts if the rate has not yet compounded to the ceiling. Those extra
	// receipts are real future value but unknowable now — retained on the
	// quote as PayoffResidualReceiptsRaw (with the post-return custody
	// leftover) for the refreshed continuation pass, never wired into this
	// recipe and never claimed as flat exit custody.
	entryUpper, err := payoffDepositReceiptsAt(reserve.totalLiquiditySF, reserve.collateralMintSupply, entryDeposit)
	if err != nil {
		return empty, err
	}
	redepositUpper, err := payoffDepositReceiptsAt(reserve.totalLiquiditySF, reserve.collateralMintSupply, redepositDeposit)
	if err != nil {
		return empty, err
	}
	ownedUpper, err := budgetSumU64(entryUpper, redepositUpper)
	if err != nil {
		return empty, err
	}
	// Explicit bound ordering: the guaranteed budget can never exceed the
	// current-rate counts because the ceiling pool is at least the current
	// pool. A violation means a units or data defect — refuse, never wrap.
	if ownedReceipts > ownedUpper {
		return empty, budgetHold("selector_destination_payoff_receipt_budget_broken")
	}
	payoff.ResidualReceiptsRaw = ownedUpper - ownedReceipts
	// Guaranteed residue input: minimum funding output minus maximum repayment.
	// The optimistic quoted output can never widen it; residue zero emits no
	// conversion evidence and no quote request at all. PYUSD minimums and
	// AUTO liquidity amounts are never subtracted from each other anywhere
	// below — each ledger runs in its own mint.
	residueProceeds := uint64(0)
	if residue := funding.Request.MinimumOutputRaw - debt; residue > 0 {
		if residue > math.MaxInt64 {
			return empty, budgetHold("selector_destination_payoff_amount_overflow")
		}
		// Convertibility is decided by the conversion quote's own enforceable
		// wire floor, never by residue units: above peg a single raw debt unit
		// can quote more than one USDC raw and stay convertible, while a
		// larger residue below peg can still floor to zero. The shared
		// producer fails closed with a typed hold exactly when the normalized
		// floor is zero; this caller propagates it instead of discarding the
		// dust or fabricating proceeds for it.
		converted, err := prepareJupiterQuoteEvidence(ctx, rpc, client, m, Decision{Action: SwapDebtToUSDCStep, AmountRaw: int64(residue), StrategyKey: route.Lane}, residue, 0, observationFloor)
		if err != nil {
			return empty, err
		}
		payoff.ResidueInputRaw = residue
		residueProceeds = converted.Request.MinimumOutputRaw
		payoff.Legs = append(payoff.Legs, converted)
	}
	// The FULL position returns, not only the LTV-bounded release remainder:
	// after the debt is repaid, the post-repay withdrawal redeems every
	// remaining GUARANTEED receipt — the exact remainder of the receipt
	// budget — and its guaranteed floored redemption is the return bound.
	// Conservation is structural: funding and return redemptions are floors
	// of one receipt budget whose deposit-time value never exceeded the
	// deposited liquidity, so total redeemed collateral can never exceed the
	// justified deposit bound, and the quote sells exactly the guaranteed
	// ledger custody, never a padded balance.
	returnReceipts := ownedReceipts - fundingReceipts
	fullReturn, err := payoffLiquidityForReceipts(reserve.totalLiquiditySF, reserve.collateralMintSupply, returnReceipts)
	if err != nil {
		return empty, err
	}
	if fullReturn > math.MaxInt64 {
		return empty, budgetHold("selector_destination_payoff_amount_overflow")
	}
	if fullReturn > 0 {
		custody, err := budgetSumU64(payoff.CustodyLeftoverRaw, fullReturn)
		if err != nil {
			return empty, err
		}
		exitReturn, err := prepareJupiterQuoteEvidence(ctx, rpc, client, m, Decision{Action: SwapCollateralToStableStep, AmountRaw: int64(fullReturn), StrategyKey: route.Lane}, custody, residueProceeds, observationFloor)
		if err != nil {
			return empty, err
		}
		payoff.ReturnAmountRaw, payoff.ReturnReceiptsRaw = fullReturn, returnReceipts
		payoff.Legs = append(payoff.Legs, exitReturn)
	}
	return payoff, nil
}

// budgetSumU64 is the overflow-checked uint64 addition used by the payoff
// ledger; every payoff quantity is an explicit bound, never a simulated state.
func budgetSumU64(a, b uint64) (uint64, error) {
	if a > math.MaxUint64-b {
		return 0, budgetHold("selector_destination_payoff_amount_overflow")
	}
	return a + b, nil
}
