package backyard

import (
	"testing"
)

// A funded debt-free OnRe position with idle Voltr cash (B4): the USDC lane
// gets the same plan B3 top-up sequence as AUTO.
func onreTopupSnapshot() Snapshot {
	s := base()
	s.RouteLane, s.StrategyKey = onreONycUSDC, onreONycUSDC
	s.HasPosition, s.PositionCollateralRaw, s.PositionCollateralValueRaw = true, 300_000_000_000, 345_000_000
	s.LTVBPS, s.LiquidationThresholdBPS = 0, 8000
	s.VoltrIdleRaw, s.TopupDepositRoomRaw = 1_295_000_000, 5_000_000_000
	s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw = 0, 0, 0 // borrow capacity is irrelevant to a top-up
	return s
}

func TestOnReTopupSequenceOnTheUSDCPath(t *testing.T) {
	t.Parallel()
	s := onreTopupSnapshot()
	check := func(s Snapshot, action Action, reason string, amount int64) {
		t.Helper()
		got := Decide(targeted(s))
		if got.Action != action || got.Reason != reason || got.AmountRaw != amount || got.StrategyKey != onreONycUSDC || got.Validate() != nil {
			t.Fatalf("want %s %s %d, got %+v", action, reason, amount, got)
		}
	}
	check(s, VoltrAllocateToSquads, topupAllocationReason, 1_295_000_000)
	s.VoltrIdleRaw, s.SquadsIdleRaw = 0, 1_295_000_000
	check(s, SwapStableToCollateralStep, topupSwapReason, 1_295_000_000)
	s.SquadsIdleRaw, s.CollateralIdleRaw, s.PrimeIdleRaw = 0, 1_120_000_000_000, 1_120_000_000_000
	check(s, OpenRouteStep, topupDepositReason, 1_120_000_000_000)
	// Once everything is deposited the B2 target borrow of the whole
	// collateral follows (no top-up is left to do).
	s.CollateralIdleRaw, s.PrimeIdleRaw, s.PositionCollateralRaw = 0, 0, 1_420_000_000_000
	s.LeverageTargetLevel = 1.5
	armLeverageCapacityFixture(&s)
	check(s, OpenRouteStep, leverageUpReason, int64(s.LeverageBorrow150Raw))
	// Size: min(idle - buffer, tranche cap, deposit-limit room).
	room := onreTopupSnapshot()
	room.TopupDepositRoomRaw = 400_000_000
	check(room, VoltrAllocateToSquads, topupAllocationReason, 400_000_000)
}

func TestTopupKeepsSafetyPriorityAndFollowsTheRegistry(t *testing.T) {
	t.Parallel()
	s := onreTopupSnapshot()
	for name, mutate := range map[string]func(*Snapshot){
		"uncovered demand": func(s *Snapshot) { s.WithdrawalDemandRaw = s.VoltrIdleRaw + 1 },
		"covered demand":   func(s *Snapshot) { s.WithdrawalDemandRaw = 1 },
		"hard ltv": func(s *Snapshot) {
			s.PositionDebtRaw, s.PositionDebtValueRaw, s.LTVBPS, s.SquadsIdleRaw = 1, 1, 6000, 1
		},
		"report due":  func(s *Snapshot) { s.PostMutationNAVRequired = true },
		"nonterminal": func(s *Snapshot) { s.Nonterminal = Built },
		"leveraged": func(s *Snapshot) {
			s.PositionDebtRaw, s.PositionDebtValueRaw, s.LTVBPS = 100_000_000, 100_000_000, 3000
		},
		"flat": func(s *Snapshot) { s.HasPosition, s.PositionCollateralRaw, s.PositionCollateralValueRaw = false, 0, 0 },
	} {
		c := s
		mutate(&c)
		switch got := Decide(targeted(c)); got.Reason {
		case topupAllocationReason, topupSwapReason, topupDepositReason:
			t.Fatalf("%s: top-up selected: %+v", name, got)
		}
	}
	// Every active registry lane tops up a debt-free position the same way
	// (B3 follows the registry): Maple swaps its Squads cash and allocates
	// later Voltr idle exactly like OnRe.
	maple := s
	maple.RouteLane, maple.StrategyKey = SelectedRouteID, SelectedRouteID
	maple.VoltrIdleRaw, maple.SquadsIdleRaw = 0, 1_000_000
	if got := Decide(targeted(maple)); got.Reason != topupSwapReason {
		t.Fatalf("Maple did not top up its Squads cash: %+v", got)
	}
	maple.SquadsIdleRaw, maple.VoltrIdleRaw = 0, 1_295_000_000
	if got := Decide(targeted(maple)); got.Reason != topupAllocationReason {
		t.Fatalf("Maple did not top up its Voltr idle: %+v", got)
	}
}
