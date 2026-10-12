package backyard

import (
	"math"
	"math/big"
)

func kaminoBorrowFeeAtRate(rate, receive uint64) (uint64, error) {
	if receive == 0 || receive > math.MaxInt64 {
		return 0, budgetHold("invalid_borrow_receive_amount")
	}
	if rate == 0 {
		return 0, nil
	}
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	feeSF := new(big.Int).Mul(new(big.Int).SetUint64(receive), new(big.Int).SetUint64(rate))
	if feeSF.Cmp(one) < 0 {
		feeSF.Set(one)
	}
	if feeSF.Cmp(new(big.Int).Mul(new(big.Int).SetUint64(receive), one)) >= 0 {
		return 0, budgetHold("borrow_below_origination_fee_minimum")
	}
	fee := feeSF.Add(feeSF, new(big.Int).Rsh(new(big.Int).Set(one), 1)).Quo(feeSF, one)
	if !fee.IsUint64() || fee.Uint64() > math.MaxInt64-receive {
		return 0, budgetHold("borrow_fee_overflow")
	}
	return fee.Uint64(), nil
}

func measureBorrowDebit(r KaminoPrimeUSDCRequest, e ExpectedEffects, route RuntimeRoute) (ExecutableDebit, error) {
	if e.Kind != "kamino-borrow" || !e.Conserved || len(e.Accounts) != 3 || e.Deposit != nil || e.Repayment != nil || e.ReturnData != nil {
		return ExecutableDebit{}, budgetHold("invalid_borrow_effect_graph")
	}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegBorrow, route)
	for i, want := range []kaminoCustodyBoundary{source, destination, {route.DebtFeeReceiver, route.Kamino.DebtMint, route.Kamino.MarketAuthority}} {
		a := e.Accounts[i]
		if a.Address != want.Address || a.Mint != want.Mint || a.Authority != want.Authority || a.Owner != route.DebtTokenProgram || a.MinimumAfterRaw != nil {
			return ExecutableDebit{}, budgetHold("invalid_borrow_effect_custody")
		}
	}
	s, d, f := e.Accounts[0], e.Accounts[1], e.Accounts[2]
	if s.BeforeRaw < s.AfterRaw || d.AfterRaw < d.BeforeRaw || f.AfterRaw < f.BeforeRaw || r.AmountRaw == 0 || d.AfterRaw-d.BeforeRaw != r.AmountRaw || r.AmountRaw > math.MaxInt64 || f.AfterRaw-f.BeforeRaw > math.MaxInt64-r.AmountRaw || s.BeforeRaw-s.AfterRaw != r.AmountRaw+f.AfterRaw-f.BeforeRaw {
		return ExecutableDebit{}, budgetHold("borrow_effect_amount_mismatch")
	}
	return ExecutableDebit{Source: s.Address, Mint: s.Mint, TokenProgram: s.Owner, Raw: s.BeforeRaw - s.AfterRaw}, nil
}
