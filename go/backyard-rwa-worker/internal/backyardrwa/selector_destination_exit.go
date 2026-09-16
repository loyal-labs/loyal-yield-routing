package backyardrwa

import (
	"context"
	"encoding/binary"
	"math"
	"math/big"
)

// Borrow -> NAV -> leverage -> NAV -> redeposit -> NAV -> release -> NAV ->
// funding -> NAV -> payoff. This is a forecast horizon, not wire freshness.
const selectorPayoffWindows int64 = 11

// Optional initializer + twelve entry messages + release/NAV/funding/NAV/payoff.
// Collateral backing accrues before the borrow too.
const selectorFullRecipeWindows int64 = 18

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

// Each of the two deposits mints floor(request/rate) receipts and transfers
// ceil(receipts*rate) liquidity, adding <1 raw unit of rounding to backing.
// Add both units before compounding, keep only the existing receipt supply,
// and round up to bound one receipt throughout the prospective exit window.
func selectorReceiptRounding(accounts []ConfirmedAccount, route RuntimeRoute, slot int64) (uint64, error) {
	reserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil || reserve.collateralMintSupply == 0 {
		return 0, budgetHold("selector_destination_receipt_bound_unavailable")
	}
	pool := new(big.Int).Add(reserve.totalLiquiditySF, new(big.Int).Lsh(big.NewInt(2), 60))
	upper, err := selectorProspectiveUpper(accounts, route, slot, route.Kamino.CollateralReserve, route.Kamino.CollateralMint, pool, selectorFullRecipeWindows)
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

func selectorDestinationExit(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, route RuntimeRoute, accounts []ConfirmedAccount, position KaminoPosition, slot, observationFloor int64, initial, redeposit, borrow, fee, rounding uint64) (uint64, JupiterExecutionEvidence, error) {
	var empty JupiterExecutionEvidence
	if initial <= 1 || redeposit <= 1 || initial-1 > math.MaxUint64-(redeposit-1) || borrow > math.MaxUint64-fee {
		return 0, empty, budgetHold("selector_destination_exit_amount_invalid")
	}
	debt, err := selectorProspectiveUpper(accounts, route, slot, route.Kamino.DebtReserve, bridgeUSDC, new(big.Int).Lsh(new(big.Int).SetUint64(borrow+fee), 60), selectorPayoffWindows)
	if err != nil {
		return 0, empty, err
	}
	market := accountAt(accounts, route.Kamino.Market).Data
	collateral := accountAt(accounts, route.Kamino.CollateralReserve).Data
	limits := kaminoPilotReleaseLimits{MaxLTVPct: collateral[kaminoLoanToValueOffset], LiquidationPct: collateral[kaminoReserveConfigOffset+17], GlobalAllowedBorrowValue: binary.LittleEndian.Uint64(market[kaminoGlobalBorrowValueOffset:])}
	copy(limits.MinimumRemainingValueSF[:], market[kaminoMinRemainingValueOffset:kaminoMinRemainingValueOffset+16])
	values := kaminoReleaseValues{CollateralRaw: initial - 1 + redeposit - 1, DebtRaw: debt, CollateralDecimals: position.CollateralDecimals, DebtDecimals: position.DebtDecimals, CollateralPriceSF: position.CollateralPriceSF, DebtPriceSF: position.DebtPriceSF}
	allowance, err := pilotRepaymentLiquidityAllowanceForValues(values, limits)
	if err != nil {
		return 0, empty, err
	}
	if rounding == 0 || rounding > math.MaxUint64/2 || allowance <= 2*rounding || allowance > math.MaxInt64 {
		return 0, empty, budgetHold("selector_destination_exit_rounding_unavailable")
	}
	amount := allowance - 2*rounding
	quote, err := prepareJupiterQuoteEvidence(ctx, rpc, client, m, Decision{Action: SwapCollateralToStableStep, AmountRaw: int64(amount), StrategyKey: route.Lane}, amount, 0, observationFloor)
	if err != nil {
		return 0, empty, err
	}
	if quote.Request.MinimumOutputRaw < debt {
		return 0, empty, budgetHold("selector_destination_payoff_quote_insufficient")
	}
	return debt, quote, nil
}
