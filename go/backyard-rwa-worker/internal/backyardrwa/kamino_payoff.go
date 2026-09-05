package backyardrwa

import (
	"context"
	"encoding/binary"
	"math"
	"math/big"
)

// This is an exit-cost estimate over a finite execution window, not a change
// to the caps or a guarantee after an arbitrary chain/price/configuration change.
// Recompute from chain Clock at final send; a now-insufficient wire remains HOLD.
const kaminoPayoffWindowSeconds int64 = 60

type KaminoPayoffBound struct {
	ObservedSlot       int64  `json:"observedSlot"`
	ChainUnix          int64  `json:"chainUnix"`
	ThroughSlot        int64  `json:"throughSlot"`
	ThroughUnix        int64  `json:"throughUnix"`
	ReserveUpdatedSlot int64  `json:"reserveUpdatedSlot"`
	ReserveUpdatedUnix int64  `json:"reserveUpdatedUnix"`
	InterestBasis      byte   `json:"interestBasis"`
	MaximumRateBPS     uint64 `json:"maximumRateBps"`
	ObservedDebtRaw    uint64 `json:"observedDebtRaw"`
	UpperDebtRaw       uint64 `json:"upperDebtRaw"`
	AccountsSHA256     string `json:"accountsSha256"`
}

func observeKaminoPayoffBound(ctx context.Context, rpc *RPCClient, route RuntimeRoute, minimumSlot int64) (KaminoPayoffBound, []ConfirmedAccount, error) {
	if rpc == nil || minimumSlot <= 0 {
		return KaminoPayoffBound{}, nil, budgetHold("payoff_observation_unavailable")
	}
	addresses := []string{route.Kamino.Obligation, route.Kamino.DebtReserve, route.DebtCustody, route.DebtLiquiditySupply,
		route.Kamino.CollateralReserve, route.CollateralCustody, route.CollateralLiquiditySupply, budgetClockAddress}
	slot, accounts, err := rpc.GetMultipleAccounts(ctx, addresses, minimumSlot)
	if err != nil {
		return KaminoPayoffBound{}, nil, err
	}
	bound, err := decodeKaminoPayoffBound(accounts, route, slot)
	return bound, accounts, err
}

func decodeKaminoPayoffBound(accounts []ConfirmedAccount, route RuntimeRoute, slot int64) (KaminoPayoffBound, error) {
	var bound KaminoPayoffBound
	clock := accountAt(accounts, budgetClockAddress)
	if slot <= 0 || slot > math.MaxInt64-budgetMaxObservationLagSlots-1 || clock.Owner != "Sysvar1111111111111111111111111111111111111" ||
		clock.Executable || len(clock.Data) != 40 {
		return bound, budgetHold("invalid_payoff_clock")
	}
	clockSlot := binary.LittleEndian.Uint64(clock.Data[:8])
	now := int64(binary.LittleEndian.Uint64(clock.Data[32:40]))
	if clockSlot < uint64(slot) || clockSlot > uint64(slot)+1 || now <= 0 || now > math.MaxInt64-kaminoPayoffWindowSeconds {
		return bound, budgetHold("invalid_payoff_clock")
	}
	reserveAccount := accountAt(accounts, route.Kamino.DebtReserve)
	reserve, err := decodeKaminoReserve(reserveAccount, route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		return bound, err
	}
	obligation, err := decodeKaminoObligation(accountAt(accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil {
		return bound, err
	}
	debt, err := obligation.debtAtReserveRate(reserve)
	if err != nil || debt == 0 || reserve.refreshedSlot <= 0 || reserve.refreshedSlot > int64(clockSlot) {
		return bound, budgetHold("payoff_debt_state_unavailable")
	}
	config := reserveAccount.Data[kaminoReserveConfigOffset:]
	// Official current KLend layout: basis at config+9 (formerly SDK padding),
	// LastUpdate timestamp at account+28, curve at config+64. Term-debt fields
	// start at config+920; those and an early-repay penalty need another model.
	// The 7.3.9 SDK does NOT decode the basis/timestamp: do not use its slot-only APR.
	basis := config[9]
	if basis > 1 || config[7] != 0 || !allZero(config[920:936]) {
		return bound, budgetHold("unsupported_payoff_interest_terms")
	}
	maximumRate, previousUtil, previousRate := uint64(0), uint32(0), uint32(0)
	for i := 0; i < 11; i++ {
		offset := 64 + i*8
		util, rate := binary.LittleEndian.Uint32(config[offset:]), binary.LittleEndian.Uint32(config[offset+4:])
		if (i == 0 && util != 0) || (i == 10 && util != 10_000) || util > 10_000 ||
			(i > 0 && ((previousUtil < 10_000 && util <= previousUtil) || util < previousUtil || rate < previousRate)) {
			return bound, budgetHold("invalid_payoff_borrow_curve")
		}
		maximumRate = max(maximumRate, uint64(rate))
		previousUtil, previousRate = util, rate
	}
	maximumRate += uint64(binary.LittleEndian.Uint16(config[2:4]))
	updatedUnix := int64(binary.LittleEndian.Uint32(reserveAccount.Data[28:32]))
	bound = KaminoPayoffBound{ObservedSlot: slot, ChainUnix: now, ThroughSlot: int64(clockSlot) + budgetMaxObservationLagSlots,
		ThroughUnix: now + kaminoPayoffWindowSeconds, ReserveUpdatedSlot: reserve.refreshedSlot, ReserveUpdatedUnix: updatedUnix,
		InterestBasis: basis, MaximumRateBPS: maximumRate, ObservedDebtRaw: debt, AccountsSHA256: hashConfirmedAccounts(accounts)}
	elapsed, unitsPerYear := bound.ThroughSlot-reserve.refreshedSlot, uint64(63_072_000)
	if basis == 1 {
		if updatedUnix <= 0 || updatedUnix > now {
			return bound, budgetHold("invalid_payoff_refresh_timestamp")
		}
		elapsed, unitsPerYear = bound.ThroughUnix-updatedUnix, 31_536_000
	}
	// Keep sub-token debt precision. Starting from ceil(raw debt) on every
	// revalidation would add a whole unit of artificial interest at each refresh.
	former := littleInt(obligation.cumulativeBorrowRate[:])
	debtSF := new(big.Int).Mul(littleInt(obligation.debtAmountSF[:]), littleInt(reserve.cumulativeBorrowRate[:]))
	debtSF.Add(debtSF, new(big.Int).Sub(former, big.NewInt(1))).Quo(debtSF, former)
	bound.UpperDebtRaw, err = upperKaminoCompoundedDebtSF(debtSF, maximumRate, uint64(elapsed), unitsPerYear)
	return bound, err
}

// KLend's nonnegative truncated binomial accrual is <= (1+rate/year)^elapsed,
// including arbitrary intermediate refreshes. Evaluate that upper bound with
// Q60 round-UP exponentiation, then round debt UP once. Rate/config changes are
// handled by reobservation, not an assumption that the current curve is permanent.
func upperKaminoCompoundedDebtSF(debtSF *big.Int, annualBPS, elapsed, unitsPerYear uint64) (uint64, error) {
	if debtSF == nil || debtSF.Sign() <= 0 || debtSF.BitLen() > 128 || unitsPerYear == 0 {
		return 0, budgetHold("invalid_payoff_interest_input")
	}
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	ceilDivide := func(n, d *big.Int) *big.Int {
		return new(big.Int).Quo(new(big.Int).Add(n, new(big.Int).Sub(d, big.NewInt(1))), d)
	}
	rate := ceilDivide(new(big.Int).Mul(new(big.Int).SetUint64(annualBPS), one), new(big.Int).Mul(new(big.Int).SetUint64(unitsPerYear), big.NewInt(10_000)))
	base, factor := new(big.Int).Add(one, rate), new(big.Int).Set(one)
	for n := elapsed; n > 0; n >>= 1 {
		if n&1 != 0 {
			factor = ceilDivide(new(big.Int).Mul(factor, base), one)
			if factor.BitLen() > 128 {
				return 0, budgetHold("payoff_interest_overflow")
			}
		}
		if n > 1 {
			base = ceilDivide(new(big.Int).Mul(base, base), one)
			if base.BitLen() > 128 {
				return 0, budgetHold("payoff_interest_overflow")
			}
		}
	}
	upper := ceilDivide(new(big.Int).Mul(debtSF, factor), new(big.Int).Mul(one, one))
	if !upper.IsUint64() || upper.Uint64() > math.MaxInt64 {
		return 0, budgetHold("payoff_interest_overflow")
	}
	return upper.Uint64(), nil
}

func validateFullPayoffRequest(ctx context.Context, rpc *RPCClient, request KaminoPrimeUSDCRequest, effects ExpectedEffects, minimumSlot int64) (KaminoPayoffBound, error) {
	if _, err := MeasureExecutableDebit(request, effects); err != nil {
		return KaminoPayoffBound{}, err
	}
	route, err := runtimeRoute(request.RouteLane)
	if err != nil {
		return KaminoPayoffBound{}, err
	}
	bound, accounts, err := observeKaminoPayoffBound(ctx, rpc, route, minimumSlot)
	if err != nil {
		return bound, err
	}
	if !request.FullPayoff || effects.Repayment == nil || request.AmountRaw < bound.UpperDebtRaw || effects.Repayment.MinimumDebitRaw > bound.ObservedDebtRaw {
		return bound, budgetHold("full_payoff_request_underfunded")
	}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegRepay, route)
	fresh, err := boundedKaminoRepaymentEffects(accounts, source, destination, effects.Repayment.MinimumDebitRaw, request.AmountRaw)
	if err != nil {
		return bound, err
	}
	for i, account := range fresh.Accounts {
		if account != effects.Accounts[i] {
			return bound, budgetHold("full_payoff_custody_changed")
		}
	}
	return bound, nil
}
