package backyardrwa

import "testing"

func base() Snapshot {
	return Snapshot{ObservationID: "o", Slot: 9, RouteKind: RouteKind, Fresh: true, LiquidationThresholdBPS: 8000, CapacityRaw: 7, PolicyLimitRaw: 10, MaxTargetLTVEntryRaw: 7, PolicyReady: true, ExitBuildable: true}
}
func TestDecisionPrecedenceAndOneAction(t *testing.T) {
	s := base()
	s.Nonterminal = Submitted
	s.HasAmbiguousSubmission = true
	if got := Decide(s); got.Action != RecoverTransaction {
		t.Fatal(got)
	}
	s = base()
	s.Nonterminal = Built
	if got := Decide(s); got.Action != RecoverTransaction || got.Reason != "resume_nonterminal_operation" {
		t.Fatal(got)
	}
	s = base()
	s.HasPosition = true
	s.PositionDebtRaw = 3
	s.SquadsIdleRaw = 3
	s.LTVBPS = 6000
	s.WithdrawalDemandRaw = 3
	if got := Decide(s); got.Action != DeleverPrimeUSDCStep {
		t.Fatal(got)
	}
	s = base()
	s.WithdrawalDemandRaw = 3
	s.SquadsIdleRaw = 5
	if got := Decide(s); got.Action != StageSquadsToVoltr || got.AmountRaw != 3 {
		t.Fatal(got)
	}
	s = base()
	s.WithdrawalDemandRaw = 8
	s.VoltrIdleRaw = 3
	s.VoltrStrategyIdleRaw = 5
	if got := Decide(s); got.Action != VoltrRestoreIdle || got.AmountRaw != 5 {
		t.Fatal(got)
	}
	s = base()
	s.WithdrawalDemandRaw = 8
	s.SquadsIdleRaw = 2
	s.HasPosition = true
	if got := Decide(s); got.Action != DeleverPrimeUSDCStep || got.AmountRaw != 6 {
		t.Fatal(got)
	}
	s = base()
	s.WithdrawalDemandRaw = 8
	s.VoltrStrategyIdleRaw = 2
	s.SquadsIdleRaw = 3
	s.HasPosition = true
	if got := Decide(s); got.Action != DeleverPrimeUSDCStep || got.AmountRaw != 3 {
		t.Fatal(got)
	}
	s = base()
	s.WithdrawalDemandRaw = 8
	s.VoltrIdleRaw = 8
	if got := Decide(s); got.Action != Hold || got.Reason != "withdrawal_covered" {
		t.Fatal(got)
	}
}
func TestDuplicateObservationIsIdempotent(t *testing.T) {
	a, b := Decide(base()), Decide(base())
	if a.IdempotencyKey != b.IdempotencyKey || a.Action != b.Action {
		t.Fatalf("%+v %+v", a, b)
	}
	later := base()
	later.Slot++
	c := Decide(later)
	if a.IdempotencyKey != c.IdempotencyKey {
		t.Fatalf("identical state at a later slot created a new identity: %s != %s", a.IdempotencyKey, c.IdempotencyKey)
	}
}

func TestIdempotencyIdentityIncludesEconomics(t *testing.T) {
	baseline := base()
	baseline.SquadsIdleRaw = 4
	a := Decide(baseline)
	changed := baseline
	changed.SquadsIdleRaw = 3
	b := Decide(changed)
	if a.IdempotencyKey == b.IdempotencyKey || a.AmountRaw == b.AmountRaw {
		t.Fatalf("changed economics aliased: a=%+v b=%+v", a, b)
	}
}

func TestEntryIsBoundedAndInvalidThresholdFailsClosed(t *testing.T) {
	s := base()
	s.SquadsIdleRaw = 4
	if got := Decide(s); got.Action != SwapUSDCToPrimeStep || got.AmountRaw != 4 {
		t.Fatal(got)
	}
	s.LiquidationThresholdBPS = 6000
	if got := Decide(s); got.Action != HoldManualRecovery {
		t.Fatal(got)
	}
	s = base()
	s.SquadsIdleRaw = 9
	s.MaxTargetLTVEntryRaw = 2
	if got := Decide(s); got.Action != SwapUSDCToPrimeStep || got.AmountRaw != 2 {
		t.Fatalf("target-LTV/capacity bound was not applied: %+v", got)
	}
	s.MaxTargetLTVEntryRaw = 0
	if got := Decide(s); got.Action != Hold || got.Reason != "insufficient_reviewed_entry_capacity" {
		t.Fatalf("zero Kamino headroom was admitted: %+v", got)
	}
}

func TestSingleLoopEntryAndFullWithdrawalPrecedence(t *testing.T) {
	entry := base()
	entry.PolicyLimitRaw = 1_000
	entry.CapacityRaw = 1_000
	entry.MaxTargetLTVEntryRaw = 1_000
	entry.SquadsIdleRaw = 100
	if got := Decide(entry); got.Action != SwapUSDCToPrimeStep || got.AmountRaw != 100 {
		t.Fatal(got)
	}
	entry.SquadsIdleRaw, entry.PrimeIdleRaw = 0, 99
	if got := Decide(entry); got.Action != OpenPrimeUSDCStep || got.Reason != "prime_collateral_ready" {
		t.Fatal(got)
	}
	entry.PrimeIdleRaw, entry.HasPosition, entry.PositionCollateralRaw = 0, true, 99
	if got := Decide(entry); got.Action != OpenPrimeUSDCStep || got.Reason != "prime_collateral_requires_borrow" {
		t.Fatal(got)
	}
	entry.PositionDebtRaw, entry.SquadsIdleRaw, entry.LTVBPS = 40, 40, TargetLTVBPS
	if got := Decide(entry); got.Action != SwapUSDCToPrimeStep || got.Reason != "borrowed_usdc_requires_prime_buffer" {
		t.Fatal(got)
	}
	entry.SquadsIdleRaw, entry.PrimeIdleRaw = 0, 39
	if got := Decide(entry); got.Action != OpenPrimeUSDCStep || got.Reason != "single_loop_redeposit" || got.AmountRaw != 39 {
		t.Fatal(got)
	}
	entry.PrimeIdleRaw, entry.PositionCollateralRaw = 0, 138
	if got := Decide(entry); got.Action != Hold || got.Reason != "single_loop_position_ready" {
		t.Fatal(got)
	}

	withdraw := entry
	withdraw.WithdrawalDemandRaw = 99
	if got := Decide(withdraw); got.Action != DeleverPrimeUSDCStep || got.Reason != "withdrawal_release_repayment_collateral" {
		t.Fatal(got)
	}
	withdraw.PrimeIdleRaw, withdraw.PositionCollateralRaw = 28, 110
	if got := Decide(withdraw); got.Action != SwapPrimeToUSDCStep || got.Reason != "withdrawal_swap_repayment_buffer" || got.AmountRaw != 28 {
		t.Fatal(got)
	}
	withdraw.PrimeIdleRaw, withdraw.SquadsIdleRaw = 0, 28
	if got := Decide(withdraw); got.Action != DeleverPrimeUSDCStep || got.Reason != "withdrawal_repay_debt" || got.AmountRaw != 28 {
		t.Fatal(got)
	}
	withdraw.PositionDebtRaw, withdraw.SquadsIdleRaw = 12, 0
	if got := Decide(withdraw); got.Action != DeleverPrimeUSDCStep || got.Reason != "withdrawal_release_repayment_collateral" {
		t.Fatal(got)
	}
	// The same release→swap→repay loop repeats until debt is exactly zero.
	withdraw.PositionDebtRaw, withdraw.SquadsIdleRaw = 0, 0
	if got := Decide(withdraw); got.Action != DeleverPrimeUSDCStep || got.Reason != "withdrawal_withdraw_collateral" {
		t.Fatal(got)
	}
	withdraw.HasPosition, withdraw.PositionCollateralRaw, withdraw.PrimeIdleRaw = false, 0, 99
	if got := Decide(withdraw); got.Action != SwapPrimeToUSDCStep || got.AmountRaw != 99 {
		t.Fatal(got)
	}
	withdraw.PrimeIdleRaw, withdraw.SquadsIdleRaw = 0, 99
	if got := Decide(withdraw); got.Action != StageSquadsToVoltr || got.AmountRaw != 99 {
		t.Fatal(got)
	}
	withdraw.SquadsIdleRaw, withdraw.VoltrStrategyIdleRaw = 0, 99
	if got := Decide(withdraw); got.Action != VoltrRestoreIdle || got.AmountRaw != 99 {
		t.Fatal(got)
	}

	release := base()
	release.WithdrawalDemandRaw, release.HasPosition, release.PositionCollateralRaw, release.PositionDebtRaw = 99, true, 99, 40
	if got := Decide(release); got.Action != DeleverPrimeUSDCStep || got.Reason != "withdrawal_release_repayment_collateral" {
		t.Fatal(got)
	}
}

func TestDebtReserveUtilizationDefersBorrowWithoutBlockingWithdrawal(t *testing.T) {
	s := base()
	s.HasPosition = true
	s.PositionCollateralRaw = 99
	s.BorrowUtilizationBlocked = true
	if got := Decide(s); got.Action != Hold || got.Reason != "debt_reserve_utilization_blocks_borrow" {
		t.Fatalf("blocked debt reserve retried borrow: %+v", got)
	}
	s.WithdrawalDemandRaw = 50
	if got := Decide(s); got.Action != DeleverPrimeUSDCStep || got.Reason != "withdrawal_withdraw_collateral" {
		t.Fatalf("utilization hold blocked withdrawal unwind: %+v", got)
	}
}
