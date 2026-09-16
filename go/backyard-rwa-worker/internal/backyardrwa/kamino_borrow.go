package backyardrwa

import (
	"context"
	"encoding/binary"
	"math"
	"math/big"
	"reflect"
)

// Exact-receive borrowing adds the origination fee to both reserve debit and
// obligation debt. The installed builder has no referrer account; reject a
// referred obligation instead of mispricing its separate accrued referral fee.
// Official KLend uses max(1, receive * Q60 fee).round(), not ceil or floor.
func kaminoBorrowFee(accounts []ConfirmedAccount, route RuntimeRoute, receive uint64) (uint64, error) {
	reserve := accountAt(accounts, route.Kamino.DebtReserve)
	if _, err := decodeKaminoReserve(reserve, route.Kamino.DebtMint, route.Kamino); err != nil {
		return 0, err
	}
	if !sameKey(reserve.Data[192:224], route.DebtFeeReceiver) {
		return 0, budgetHold("borrow_fee_receiver_changed")
	}
	obligation := accountAt(accounts, route.Kamino.Obligation)
	if _, err := decodeKaminoObligation(obligation, route.Kamino); err != nil {
		return 0, err
	}
	if !allZero(obligation.Data[2288:2320]) {
		return 0, budgetHold("unsupported_borrow_referrer")
	}
	rate := binary.LittleEndian.Uint64(reserve.Data[kaminoReserveConfigOffset+40:])
	return kaminoBorrowFeeAtRate(rate, receive)
}

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

func kaminoBorrowEffects(accounts []ConfirmedAccount, route RuntimeRoute, receive uint64) (ExpectedEffects, error) {
	fee, err := kaminoBorrowFee(accounts, route, receive)
	if err != nil {
		return ExpectedEffects{}, err
	}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegBorrow, route)
	effects, err := exactKaminoTokenEffects(accounts, source, destination, receive)
	if err != nil {
		return effects, err
	}
	if effects.Accounts[0].AfterRaw < fee {
		return effects, budgetHold("borrow_supply_cannot_cover_fee")
	}
	a := accountAt(accounts, route.DebtFeeReceiver)
	mint, _ := decodeBase58PublicKey(route.Kamino.DebtMint)
	authority, _ := decodeBase58PublicKey(route.Kamino.MarketAuthority)
	custody, err := DecodeTokenCustody(a.Owner, a.Data, mint, authority)
	if err != nil || a.Owner != route.DebtTokenProgram || a.Lamports == 0 || a.Executable || custody.Raw > math.MaxUint64-fee {
		return effects, budgetHold("invalid_borrow_fee_custody")
	}
	effects.Kind = "kamino-borrow"
	effects.Accounts[0].AfterRaw -= fee
	effects.Accounts = append(effects.Accounts, ExpectedAccountEffect{Address: a.Address, Owner: a.Owner, Mint: route.Kamino.DebtMint, Authority: route.Kamino.MarketAuthority, BeforeRaw: custody.Raw, AfterRaw: custody.Raw + fee})
	return effects, nil
}

// Fresh fee/config/custody validation runs at build and persisted-input send.
// Reconciliation already checks exact three-account conservation per receipt.
func validateBorrowRequest(ctx context.Context, rpc *RPCClient, r KaminoPrimeUSDCRequest, e ExpectedEffects, slot int64) (int64, error) {
	if _, err := MeasureExecutableDebit(r, e); err != nil {
		return 0, err
	}
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		return 0, err
	}
	observed, accounts, err := rpc.GetMultipleAccounts(ctx, []string{route.Kamino.DebtReserve, route.Kamino.Obligation, route.DebtLiquiditySupply, route.DebtCustody, route.DebtFeeReceiver}, slot)
	if err != nil {
		return 0, err
	}
	fresh, err := kaminoBorrowEffects(accounts, route, r.AmountRaw)
	if err != nil {
		return 0, err
	}
	if !reflect.DeepEqual(fresh, e) {
		return 0, budgetHold("borrow_fee_or_custody_changed")
	}
	return observed, nil
}

func measureBorrowDebit(r KaminoPrimeUSDCRequest, e ExpectedEffects, route RuntimeRoute) (ExecutableDebit, error) {
	if r.FullPayoff || r.RepaymentRelease || e.Kind != "kamino-borrow" || !e.Conserved || len(e.Accounts) != 3 || e.Deposit != nil || e.Repayment != nil || e.ReturnData != nil {
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
