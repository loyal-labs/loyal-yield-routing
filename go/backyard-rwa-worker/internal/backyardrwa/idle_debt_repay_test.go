package backyardrwa

import "testing"

// Live 2026-09-28 AUTO state: a cancelled exit left 226.44 PYUSD in debt
// custody against 189.92 PYUSD debt at 55% LTV, with no withdrawal demand.
func liveIdleDebtSnapshot() Snapshot {
	s := base()
	s.RouteLane = autoAUTOPYUSD.Lane
	s.HasPosition, s.PositionCollateralRaw, s.PositionDebtRaw = true, 338_210_000, 189_920_000
	s.PositionCollateralValueRaw, s.PositionDebtValueRaw = 345_000_000, 189_920_000
	s.PayoffDebtRaw, s.DebtIdleRaw, s.LTVBPS = 189_921_000, 226_440_000, 5500
	s.VoltrIdleRaw = 1_295_000_000
	return s
}

func TestIdleDebtBufferRepaysWholeDebtWithoutDemand(t *testing.T) {
	s := liveIdleDebtSnapshot()
	if got := Decide(s); got.Action != DeleverRouteStep || got.Reason != "idle_debt_repay" || got.AmountRaw != s.PositionDebtRaw || got.Validate() != nil {
		t.Fatalf("live stuck state did not repay the debt: %+v", got)
	}
	// A normal borrow never leaves more debt cash than debt: keep leveraging.
	normal := s
	normal.DebtIdleRaw = normal.PositionDebtRaw
	if got := Decide(normal); got.Action != SwapDebtToCollateralStep {
		t.Fatalf("normal borrow proceeds were repaid: %+v", got)
	}
	// A buffer below the interest-window payoff cannot fund a full payoff.
	short := s
	short.PayoffDebtRaw = short.DebtIdleRaw + 1
	if got := Decide(short); got.Reason == "idle_debt_repay" {
		t.Fatalf("underfunded payoff selected: %+v", got)
	}
	// Hard LTV and withdrawal demand keep priority.
	hard := s
	hard.LTVBPS = 6000
	if got := Decide(hard); got.Reason != "hard_ltv_repay" {
		t.Fatalf("hard LTV lost priority: %+v", got)
	}
	demand := s
	demand.Unwind = true
	if got := Decide(demand); got.Reason != "withdrawal_repay_debt" {
		t.Fatalf("unwind lost priority: %+v", got)
	}
	// After the payoff the residue holds; no borrow is attempted.
	after := s
	after.PositionDebtRaw, after.PositionDebtValueRaw, after.PayoffDebtRaw, after.DebtIdleRaw, after.LTVBPS = 0, 0, 0, 36_520_000, 0
	if got := Decide(after); got.Action != Hold || got.Reason != "idle_debt_residue_after_repay" {
		t.Fatalf("payoff residue attempted a borrow: %+v", got)
	}
}
