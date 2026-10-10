package backyard

import (
	"math/big"
	"testing"
)

// The pre-check on the live post-borrow shape (IMG55, 20:20): 1772.66 AUTO
// @ 1.021 ($1,809.88), debt 602.87 + 302.40 borrowed = 905.26 PYUSD, the
// 302.40 still in debt custody. The old check netted the cash against the
// debt before sizing the release ($664 release, "covered"); the real safe
// release sits on the full debt ($164) and cannot fund the payoff.
func TestLeverageExitPreCheckSizesTheReleaseOnTheFullDebt(t *testing.T) {
	t.Parallel()
	c, d, cash := big.NewInt(1_809_882_868), big.NewInt(905_262_160), big.NewInt(302_395_505)
	if !leverageExitOneReleaseMayNotCover(c, big.NewInt(0), d, cash) {
		t.Fatal("live post-borrow 1.75x judged coverable by one release")
	}
	// The same position at 1.5x with no cash: one release covers.
	if leverageExitOneReleaseMayNotCover(big.NewInt(1_809_882_868), big.NewInt(0), big.NewInt(602_866_655), big.NewInt(0)) {
		t.Fatal("1.5x flagged for cycles")
	}
	s := base()
	s.RouteLane, s.StrategyKey = autoAUTOPYUSD.Lane, autoAUTOPYUSD.Lane
	s.PositionCollateralValueRaw, s.PositionDebtValueRaw, s.DebtIdleRaw = 1_809_882_868, 905_262_160, 302_395_505
	if !leverageExitMayNeedCycles(s) {
		t.Fatal("snapshot pre-check missed the post-borrow cycle")
	}
}

// The receipt brackets only this transaction, so moves on any account between
// observation and landing never fail reconciliation; this transaction's own
// delta on every account, the fee included, still must match.
func TestBorrowReceiptReconcilesDespiteSharedReserveMoves(t *testing.T) {
	t.Parallel()
	route := ethenaUSDePYUSD
	e := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-borrow", Conserved: true, Accounts: []ExpectedAccountEffect{
		{Address: route.DebtLiquiditySupply, Owner: route.DebtTokenProgram, Mint: route.Kamino.DebtMint, Authority: route.Kamino.MarketAuthority, BeforeRaw: 1_000, AfterRaw: 896},
		{Address: route.DebtCustody, Owner: route.DebtTokenProgram, Mint: route.Kamino.DebtMint, Authority: bridgeVault, BeforeRaw: 0, AfterRaw: 100},
		{Address: route.DebtFeeReceiver, Owner: route.DebtTokenProgram, Mint: route.Kamino.DebtMint, Authority: route.Kamino.MarketAuthority, BeforeRaw: 50, AfterRaw: 54},
	}}
	// Other borrowers and depositors moved both shared accounts first.
	receipt := receiptFor(e, map[string][2]uint64{route.DebtLiquiditySupply: {5_000, 4_896}, route.DebtFeeReceiver: {70, 74}})
	if _, _, err := ReconcileConfirmedTransaction(e, receipt); err != nil {
		t.Fatal("shared reserve moves before landing failed reconciliation", err)
	}
	receipt.PostTokenBalances[2].Raw++
	if _, _, err := ReconcileConfirmedTransaction(e, receipt); err == nil {
		t.Fatal("an unconserved fee reconciled")
	}
	receipt = receiptFor(e, map[string][2]uint64{route.DebtCustody: {1, 101}})
	if _, _, err := ReconcileConfirmedTransaction(e, receipt); err != nil {
		t.Fatal("a transfer into our debt custody before landing failed reconciliation", err)
	}
	// Kamino raised the fee after we prepared: conserved, but not what we signed for.
	receipt = receiptFor(e, map[string][2]uint64{route.DebtLiquiditySupply: {1_000, 890}, route.DebtFeeReceiver: {50, 60}})
	if _, _, err := ReconcileConfirmedTransaction(e, receipt); err == nil {
		t.Fatal("a borrow charged above the prepared fee reconciled")
	}
}

// receiptFor renders the expected effects as a transaction receipt, with
// optional per-address pre/post overrides.
func receiptFor(e ExpectedEffects, override map[string][2]uint64) ConfirmedTransactionEvidence {
	receipt := ConfirmedTransactionEvidence{Signature: "receipt-fixture", Slot: 42}
	for _, a := range e.Accounts {
		pre, post := a.BeforeRaw, a.AfterRaw
		if o, ok := override[a.Address]; ok {
			pre, post = o[0], o[1]
		}
		balance := TransactionTokenBalance{Address: a.Address, OwnerProgram: a.Owner, Mint: a.Mint, Authority: a.Authority}
		balance.Raw = pre
		receipt.PreTokenBalances = append(receipt.PreTokenBalances, balance)
		balance.Raw = post
		receipt.PostTokenBalances = append(receipt.PostTokenBalances, balance)
	}
	return receipt
}
