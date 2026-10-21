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
	s.CollateralIdleRaw, s.PostMutationNAVRequired = 1, true
	if got := Decide(s); got.Action != ReportNAV {
		t.Fatal("deposit bypassed NAV", got)
	}
	// Exit-only Ethena never levers: the residue beside its position holds.
	s.PostMutationNAVRequired = false
	if got := Decide(targeted(s)); got.Reason != holdAtTarget {
		t.Fatal("rounding residue restarted the loop", got)
	}
	s.PositionDebtRaw, s.PositionDebtValueRaw, s.DebtIdleRaw = 1004, 2008, 1000
	if got := Decide(targeted(s)); got.Reason != holdAtTarget {
		t.Fatal("debt residue at dust converted outside a step", got)
	}
	exit := targetInputs{Exits: map[string]laneExit{ethenaUSDePYUSD.Lane: {EquityZero: true}}}
	if got := Decide(targetedWith(s, exit)); got.Reason != debtZeroReason {
		t.Fatal("rounding rule blocked exit", got)
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
			s.CollateralIdleRaw = 1
			s.PostMutationNAVRequired = true
			if got := Decide(s); got.Action != ReportNAV {
				t.Fatal("remainder bypassed required NAV", got)
			}
			s.PostMutationNAVRequired = false
			if got := Decide(targeted(s)); got.Action != Hold || got.Reason != holdAtTarget {
				t.Fatal("B2: a debt-free position without a level borrowed", got)
			}
			if earnActiveLane(lane) {
				s.LeverageTargetLevel = 1.5
				armLeverageCapacityFixture(&s)
				if got := Decide(targeted(s)); got.Action != OpenRouteStep || got.Reason != leverageUpReason {
					t.Fatal("deposit remainder blocked borrowing", got)
				}
			}
			// At the level the remainder beside debt starts no leg: the next
			// step's deposit sweeps it.
			s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw = 33_000_000, 33_000_000, 33_000_001
			s.LTVBPS = s.PositionDebtValueRaw * 10_000 / s.PositionCollateralValueRaw
			if got := Decide(targeted(s)); got.Action != Hold {
				t.Fatal("remainder restarted finished loop", got)
			}
			exit := targetInputs{Exits: map[string]laneExit{lane: {EquityZero: true}}}
			if got := Decide(targetedWith(s, exit)); got.Action != DeleverRouteStep {
				t.Fatal("rounding threshold blocked exit", got)
			}
		})
	}
}
