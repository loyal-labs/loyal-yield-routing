package backyard

import (
	"testing"
)

func TestTopupDepositDecisionJoinsDebtFreePosition(t *testing.T) {
	t.Parallel()
	s := liveTopupSnapshot()
	s.VoltrIdleRaw, s.CollateralIdleRaw, s.PrimeIdleRaw, s.MinimumCollateralDepositRaw = 0, 300_000_000, 300_000_000, 1
	got := Decide(targeted(s))
	if got.Action != OpenRouteStep || got.Reason != topupDepositReason || got.AmountRaw != s.CollateralIdleRaw || got.Validate() != nil {
		t.Fatalf("top-up collateral was not deposited: %+v", got)
	}
	// Idle Voltr cash waits until this deposit lands (one tranche at a time).
	s.VoltrIdleRaw = 1_295_000_000
	if got := Decide(targeted(s)); got.Reason != topupDepositReason {
		t.Fatalf("allocation jumped ahead of the pending deposit: %+v", got)
	}
	// Collateral KLend would refuse to deposit is vault cash: the top-up
	// goes on, and its deposit takes that custody whole.
	below := s
	below.MinimumCollateralDepositRaw = below.CollateralIdleRaw + 1
	if got := Decide(targeted(below)); got.Action != VoltrAllocateToSquads || got.Reason != topupAllocationReason {
		t.Fatalf("collateral below the deposit minimum held the top-up: %+v", got)
	}
	for name, mutate := range map[string]func(*Snapshot){
		"demand":     func(s *Snapshot) { s.WithdrawalDemandRaw = 1 },
		"hard ltv":   func(s *Snapshot) { s.PositionDebtRaw, s.PositionDebtValueRaw, s.LTVBPS, s.DebtIdleRaw = 1, 1, 6000, 1 },
		"report due": func(s *Snapshot) { s.PostMutationNAVRequired = true },
	} {
		c := s
		mutate(&c)
		if got := Decide(targeted(c)); got.Reason == topupDepositReason {
			t.Fatalf("%s lost priority to the top-up deposit: %+v", name, got)
		}
	}
	// Beside AUTO's PYUSD debt the same deposit joins the position (it only
	// lowers LTV).
	debt := s
	debt.PositionDebtRaw, debt.PositionDebtValueRaw, debt.LTVBPS = 100_000_000, 100_000_000, 3000
	if got := Decide(targeted(debt)); got.Reason != topupDepositReason || got.AmountRaw != debt.CollateralIdleRaw {
		t.Fatalf("collateral beside debt was not deposited: %+v", got)
	}
}
