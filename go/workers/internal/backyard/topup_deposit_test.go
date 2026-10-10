package backyard

import (
	"testing"
)

func TestTopupDepositDecisionJoinsDebtFreePosition(t *testing.T) {
	s := liveTopupSnapshot()
	s.VoltrIdleRaw, s.CollateralIdleRaw, s.PrimeIdleRaw, s.MinimumCollateralDepositRaw = 0, 300_000_000, 300_000_000, 1
	got := Decide(s)
	if got.Action != OpenRouteStep || got.Reason != topupDepositReason || got.AmountRaw != s.CollateralIdleRaw || got.Validate() != nil {
		t.Fatalf("top-up collateral was not deposited: %+v", got)
	}
	// Idle Voltr cash waits until this deposit lands (one tranche at a time).
	s.VoltrIdleRaw = 1_295_000_000
	if got := Decide(s); got.Reason != topupDepositReason {
		t.Fatalf("allocation jumped ahead of the pending deposit: %+v", got)
	}
	below := s
	below.MinimumCollateralDepositRaw = below.CollateralIdleRaw + 1
	if got := Decide(below); got.Reason == topupDepositReason || got.Reason == topupAllocationReason {
		t.Fatalf("deposit below the rounding window or allocation beside it: %+v", got)
	}
	for name, mutate := range map[string]func(*Snapshot){
		"demand":     func(s *Snapshot) { s.WithdrawalDemandRaw = 1 },
		"unwind":     func(s *Snapshot) { s.Unwind = true },
		"hard ltv":   func(s *Snapshot) { s.PositionDebtRaw, s.PositionDebtValueRaw, s.LTVBPS, s.DebtIdleRaw = 1, 1, 6000, 1 },
		"report due": func(s *Snapshot) { s.PostMutationNAVRequired = true },
	} {
		c := s
		mutate(&c)
		if got := Decide(c); got.Reason == topupDepositReason {
			t.Fatalf("%s lost priority to the top-up deposit: %+v", name, got)
		}
	}
	// With debt the existing redeposit keeps its own reason (B3 stays off).
	debt := s
	debt.PositionDebtRaw, debt.PositionDebtValueRaw, debt.LTVBPS = 100_000_000, 100_000_000, 3000
	if got := Decide(debt); got.Reason != "single_loop_redeposit" {
		t.Fatalf("leveraged position used the top-up deposit: %+v", got)
	}
}
