package backyardrwa

import (
	"context"
	"encoding/binary"
	"math"
)

// KLend mints floor(request / receipt exchange rate), then transfers the ceil
// liquidity value of those receipts. Lost input is less than one receipt's
// liquidity value. Compound the ENTIRE net pool at its maximum configured
// borrow rate through the same 60-second/32-slot send window: this overestimates
// interest (only the borrowed part earns it; protocol fees reduce it). The bound
// is rounding tolerance, not permission to reduce the cap debit or report exact
// receipt issuance from stale pre-refresh reserve bytes.
func kaminoDepositMinimum(accounts []ConfirmedAccount, route RuntimeRoute, slot int64, maximum uint64) (uint64, error) {
	a := accountAt(accounts, route.Kamino.CollateralReserve)
	r, err := decodeKaminoReserve(a, route.Kamino.CollateralMint, route.Kamino)
	if err != nil || r.collateralMintSupply == 0 || maximum == 0 || maximum > math.MaxInt64 {
		return 0, budgetHold("deposit_rounding_reserve_unavailable")
	}
	clock := accountAt(accounts, budgetClockAddress)
	if slot <= 0 || slot > math.MaxInt64-budgetMaxObservationLagSlots-1 || clock.Owner != "Sysvar1111111111111111111111111111111111111" || clock.Executable || len(clock.Data) != 40 {
		return 0, budgetHold("invalid_deposit_clock")
	}
	clockSlot := binary.LittleEndian.Uint64(clock.Data[:8])
	now := int64(binary.LittleEndian.Uint64(clock.Data[32:40]))
	if clockSlot < uint64(slot) || clockSlot > uint64(slot)+1 || now <= 0 || now > math.MaxInt64-kaminoPayoffWindowSeconds || r.refreshedSlot <= 0 || r.refreshedSlot > int64(clockSlot) {
		return 0, budgetHold("invalid_deposit_clock")
	}
	config := a.Data[kaminoReserveConfigOffset:]
	if config[9] > 1 || config[7] != 0 || !allZero(config[920:936]) {
		return 0, budgetHold("unsupported_deposit_interest_terms")
	}
	rate, err := kaminoMaximumBorrowRate(config)
	if err != nil {
		return 0, err
	}
	elapsed, units := int64(clockSlot)+budgetMaxObservationLagSlots-r.refreshedSlot, uint64(63_072_000)
	if config[9] == 1 {
		updated := int64(binary.LittleEndian.Uint32(a.Data[28:32]))
		if updated <= 0 || updated > now {
			return 0, budgetHold("invalid_deposit_refresh_timestamp")
		}
		elapsed, units = now+kaminoPayoffWindowSeconds-updated, 31_536_000
	}
	upper, err := upperKaminoCompoundedDebtSF(r.totalLiquiditySF, rate, uint64(elapsed), units)
	if err != nil {
		return 0, err
	}
	loss := upper / r.collateralMintSupply
	if upper%r.collateralMintSupply != 0 {
		loss++
	}
	if loss == 0 || loss >= maximum {
		return 0, budgetHold("deposit_below_rounding_window")
	}
	return maximum - loss, nil
}

func boundedKaminoDepositEffects(accounts []ConfirmedAccount, route RuntimeRoute, slot int64, maximum uint64) (ExpectedEffects, error) {
	minimum, err := kaminoDepositMinimum(accounts, route, slot, maximum)
	if err != nil {
		return ExpectedEffects{}, err
	}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegDeposit, route)
	effects, err := exactKaminoTokenEffects(accounts, source, destination, maximum)
	if err != nil {
		return effects, err
	}
	effects.Kind, effects.Deposit = "kamino-deposit", &ExpectedDeposit{minimum, maximum}
	return effects, validateRepaymentEffects(effects)
}

func validateDepositRequest(ctx context.Context, rpc *RPCClient, request KaminoPrimeUSDCRequest, effects ExpectedEffects, slot int64) (int64, error) {
	if _, err := MeasureExecutableDebit(request, effects); err != nil {
		return 0, err
	}
	route, err := runtimeRoute(request.RouteLane)
	if err != nil {
		return 0, err
	}
	observed, accounts, err := rpc.GetMultipleAccounts(ctx, []string{route.Kamino.CollateralReserve, route.CollateralCustody, route.CollateralLiquiditySupply, budgetClockAddress}, slot)
	if err != nil {
		return 0, err
	}
	fresh, err := boundedKaminoDepositEffects(accounts, route, observed, request.AmountRaw)
	if err != nil {
		return 0, err
	}
	if effects.Deposit == nil || fresh.Deposit.MinimumDebitRaw < effects.Deposit.MinimumDebitRaw {
		return 0, budgetHold("deposit_rounding_window_changed")
	}
	for i, account := range fresh.Accounts {
		if account != effects.Accounts[i] {
			return 0, budgetHold("deposit_custody_changed")
		}
	}
	return observed, nil
}
