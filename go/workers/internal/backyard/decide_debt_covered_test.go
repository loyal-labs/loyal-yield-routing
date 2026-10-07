package backyard

import "testing"

// Live 2026-09-28: a $5 claim against $1,295 Voltr idle started a full AUTO
// unwind. Covered demand on a debt lane must report or hold, never unwind.
func TestNonUSDCCoveredWithdrawalDoesNotUnwind(t *testing.T) {
	s := base()
	s.RouteLane = autoAUTOPYUSD.Lane
	s.HasPosition, s.PositionCollateralRaw, s.PositionDebtRaw = true, 560_000_000, 190_000_000
	s.PositionCollateralValueRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw = 571_000_000, 190_000_000, 190_001_000
	s.LTVBPS, s.WithdrawalDemandRaw, s.VoltrIdleRaw = 3322, 5_000_000, 1_295_000_000
	if got := Decide(s); got.Action != Hold || got.Reason != "withdrawal_covered" {
		t.Fatalf("covered demand unwound: %+v", got)
	}
	s.LastReportAgeSeconds = 60
	if got := Decide(s); got.Action != ReportNAV || got.Reason != "withdrawal_covered_nav_due" {
		t.Fatalf("covered demand skipped its report: %+v", got)
	}
	// Uncovered demand, an admitted unwind and hard LTV still exit.
	s.LastReportAgeSeconds = 0
	uncovered := s
	uncovered.VoltrIdleRaw = 4_999_999
	if got := Decide(uncovered); got.Action != DeleverRouteStep || got.Reason != "withdrawal_release_repayment_collateral" {
		t.Fatalf("uncovered demand did not unwind: %+v", got)
	}
	unwind := s
	unwind.Unwind = true
	if got := Decide(unwind); got.Action != DeleverRouteStep {
		t.Fatalf("admitted unwind was held: %+v", got)
	}
	hard := s
	hard.LTVBPS, hard.DebtIdleRaw = 6000, 1
	if got := Decide(hard); got.Reason != "hard_ltv_repay" {
		t.Fatalf("hard LTV lost priority: %+v", got)
	}
}
