package backyard

import (
	"math"
	"testing"
)

func TestDepositRemainderDoesNotRestartEntryLoop(t *testing.T) {
	t.Parallel()
	_, _, _, _, _, _, accounts := depositAdmissionFixtureForPosition(t, "", true)
	minimum, err := kaminoDepositMinimum(accounts, ethenaUSDePYUSD, 42, math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	s := base()
	s.MinimumCollateralDepositRaw = math.MaxInt64 - int64(minimum) + 1
	if s.MinimumCollateralDepositRaw != 3 {
		t.Fatal("unexpected current rounding bound", s.MinimumCollateralDepositRaw)
	}
	if _, err := kaminoDepositMinimum(accounts, ethenaUSDePYUSD, 42, 2); err == nil {
		t.Fatal("planner threshold differs from builder")
	}
	if _, err := kaminoDepositMinimum(accounts, ethenaUSDePYUSD, 42, 3); err != nil {
		t.Fatal(err)
	}
	s.RouteLane, s.StrategyKey = ethenaUSDePYUSD.Lane, ethenaUSDePYUSD.Lane
	s.HasPosition, s.PositionCollateralRaw, s.PositionCollateralValueRaw = true, 100_000_000, 110_000
	s.CollateralIdleRaw, s.PrimeIdleRaw, s.PostMutationNAVRequired = 1, 1, true
	if got := Decide(s); got.Action != ReportNAV {
		t.Fatal("deposit bypassed NAV", got)
	}
	// Exit-only Ethena never levers: the residue beside its position holds.
	s.PostMutationNAVRequired = false
	if got := Decide(s); got.Reason != "single_loop_position_ready" {
		t.Fatal("rounding residue restarted the loop", got)
	}
	s.PositionDebtRaw, s.PositionDebtValueRaw, s.DebtIdleRaw = 1004, 2008, 1000
	if got := Decide(s); got.Reason != "single_loop_position_ready" {
		t.Fatal("debt residue converted outside a step", got)
	}
	s.CutoverDrain = true
	if got := Decide(s); got.Reason != withdrawalDeleverReason {
		t.Fatal("rounding rule blocked exit", got)
	}
	s.CutoverDrain, s.MinimumCollateralDepositRaw = false, 0
	if got := Decide(s); got.Reason != "deposit_rounding_window_unavailable" {
		t.Fatal("missing bound treated as dust", got)
	}
}

func TestPilotUSDCRoundingRemainderDoesNotRestartEntry(t *testing.T) {
	t.Parallel()
	for _, lane := range basicLaneIDs() {
		t.Run(lane, func(t *testing.T) {
			s := base()
			s.RouteLane, s.StrategyKey = lane, lane
			s.MinimumCollateralDepositRaw = 3
			s.HasPosition, s.PositionCollateralRaw, s.PositionCollateralValueRaw = true, 100_000_000, 100_000_000
			s.CollateralIdleRaw, s.PrimeIdleRaw = 1, 1
			s.PostMutationNAVRequired = true
			if got := Decide(s); got.Action != ReportNAV {
				t.Fatal("remainder bypassed required NAV", got)
			}
			s.PostMutationNAVRequired = false
			want := "prime_collateral_requires_borrow"
			if earnActiveLane(lane) {
				if got := Decide(s); got.Action != Hold || got.Reason != "leverage_target_required" {
					t.Fatal("B2: a debt-free position without a target borrowed", got)
				}
				s.LeverageTargetLevel, want = 1.5, leverageUpReason
				armLeverageCapacityFixture(&s)
			}
			if got := Decide(s); got.Action != OpenRouteStep || got.Reason != want {
				t.Fatal("deposit remainder blocked borrowing", got)
			}
			// At the target level the remainder beside debt starts no leg:
			// the next step's deposit sweeps it.
			s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw = 33_000_000, 33_000_000, 33_000_001
			s.LTVBPS = s.PositionDebtValueRaw * 10_000 / s.PositionCollateralValueRaw
			if got := Decide(s); got.Action != Hold {
				t.Fatal("remainder restarted finished loop", got)
			}
			s.Unwind = true
			if got := Decide(s); got.Action != DeleverRouteStep {
				t.Fatal("rounding threshold blocked exit", got)
			}
			s.Unwind = false
			s.MinimumCollateralDepositRaw = 0
			if got := Decide(s); got.Action != Hold || got.Reason != "deposit_rounding_window_unavailable" {
				t.Fatal("missing threshold treated as dust", got)
			}
		})
	}
}
