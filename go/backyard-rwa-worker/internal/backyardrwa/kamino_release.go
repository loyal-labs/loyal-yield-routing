package backyardrwa

import (
	"bytes"
	"context"
	"math/big"
)

type KaminoReleaseBound struct {
	Payoff              KaminoPayoffBound `json:"payoff"`
	ReceiptRaw          uint64            `json:"receiptRaw"`
	LiquidityRaw        uint64            `json:"liquidityRaw"`
	RemainingReceiptRaw uint64            `json:"remainingReceiptRaw"`
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
	var result KaminoReleaseBound
	bound, err := decodeKaminoPayoffWindow(accounts, route, slot, 5)
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
	var result KaminoReleaseBound
	if !request.RepaymentRelease || request.FullPayoff {
		return result, nil, budgetHold("invalid_repayment_release_intent")
	}
	if _, err := MeasureExecutableDebit(request, effects); err != nil {
		return result, nil, err
	}
	route, err := runtimeRoute(request.RouteLane)
	if err != nil {
		return result, nil, err
	}
	observed, accounts, err := observeKaminoPayoffWindow(ctx, rpc, route, slot, 5)
	if err != nil {
		return result, nil, err
	}
	result, err = decodeKaminoRepaymentRelease(accounts, route, observed.ObservedSlot)
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
