package backyard

import (
	"math"
	"testing"
)

// A flat borrow curve at the given APR and a native yield; spread = yield -
// expm1(apr).
func leverageMarket(lane string, yield, borrowAPR float64) LaneEconomics {
	bps := borrowAPR * 10_000
	return LaneEconomics{Lane: lane, NativeAPY: yield, CurrentBorrowAPY: math.Expm1(borrowAPR), BorrowCurve: []BorrowCurvePoint{{0, bps}, {10_000, bps}}, DebtSupplyRaw: 1e15, DebtBorrowRaw: 1e14}
}

// A settled funded AUTO position at the given level, equity ~$1,000.
func leverageSnapshot(level float64) Snapshot {
	s := base()
	s.RouteLane, s.StrategyKey = autoAUTOPYUSD.Lane, autoAUTOPYUSD.Lane
	s.HasPosition = true
	s.PositionCollateralValueRaw = int64(1_000_000_000 * level)
	s.PositionCollateralRaw = s.PositionCollateralValueRaw
	s.PositionDebtValueRaw = s.PositionCollateralValueRaw - 1_000_000_000
	s.PositionDebtRaw = s.PositionDebtValueRaw
	s.LTVBPS = leverageLevelLTVBPS(level)
	s.LeverageTargetLevel = level
	armLeverageCapacityFixture(&s)
	return s
}

func TestDecideLeverageTargetStepsAtEachLevel(t *testing.T) {
	t.Parallel()
	keep := SelectorResult{Action: "KEEP"}
	p := DefaultSelectorPolicy()
	for _, tc := range []struct {
		name       string
		level      float64
		yield, apr float64
		want       float64
		wantReason string
	}{
		{"1x stays below 1 pt", 1, 0.10, math.Log1p(0.095), 1, "spread_rule"},
		{"1x up with fee-reserved edge", 1, 0.60, math.Log1p(0.06), 1.5, "up_move_approved"},
		{"1.5x stays 1-2 pt", 1.5, 0.10, math.Log1p(0.085), 1.5, "spread_rule"},
		// Live levels reach 1.75x with the multi-cycle exit; option 1 exactly.
		{"1.5x qualifying capped loan", 1.5, 0.12, math.Log1p(0.06), 1.75, "up_move_approved"},
		{"1.5x down below 0", 1.5, 0.05, math.Log1p(0.06), 1, "spread_rule"},
		{"1.75x stays in the gap", 1.75, 0.10, math.Log1p(0.085), 1.75, "spread_rule"},
		{"1.75x down below 1 pt", 1.75, 0.10, math.Log1p(0.095), 1.5, "spread_rule"},
		{"1.75x up one level", 1.75, 0.20, math.Log1p(0.01), 2, "up_move_approved"},
	} {
		s := leverageSnapshot(tc.level)
		s.TotalVaultNAVRaw, s.StrategyNAVRaw = 1_000_000_000, 1_000_000_000
		armFeeAuthorityFixture(t, &s)
		got, ok := decideLeverageTarget(s, keep, []LaneEconomics{leverageMarket(s.RouteLane, tc.yield, tc.apr)}, p, twentyBPSStepCost)
		if !ok || got.Current != min(tc.level, laneMaxLevel(s.RouteLane)) || got.Next != tc.want || got.Reason != tc.wantReason {
			t.Fatalf("%s: %+v ok=%t", tc.name, got, ok)
		}
	}
	// Up needs the move to beat MinimumBenefit plus cost: a tiny position
	// with a qualifying spread stays put, and the numbers are logged.
	small := leverageSnapshot(1)
	small.PositionCollateralValueRaw, small.PositionCollateralRaw = 2_000_000, 2_000_000
	small.TotalVaultNAVRaw, small.StrategyNAVRaw = 2_000_000, 2_000_000
	armFeeAuthorityFixture(t, &small)
	armLeverageCapacityFixture(&small)
	got, _ := decideLeverageTarget(small, keep, []LaneEconomics{leverageMarket(small.RouteLane, 0.12, math.Log1p(0.06))}, p, twentyBPSStepCost)
	if got.Next != 1 || got.BorrowRaw != 0 {
		t.Fatalf("sub-minimum loan authorized: %+v", got)
	}
}

// The level decision never runs beside a selector entry or any unfinished
// work of its lane.
func TestLeverageDecisionRunsOnlyOnASettledLane(t *testing.T) {
	t.Parallel()
	markets := []LaneEconomics{leverageMarket(autoAUTOPYUSD.Lane, 0.12, math.Log1p(0.06))}
	p := DefaultSelectorPolicy()
	s := leverageSnapshot(1.5)
	if _, ok := decideLeverageTarget(s, SelectorResult{Action: "KEEP"}, markets, p, twentyBPSStepCost); !ok {
		t.Fatal("settled KEEP position gave no leverage decision")
	}
	// Custody beside the position is vault cash its next step takes: a
	// step's residue, even 1 raw, freezes no level.
	residue := s
	residue.DebtIdleRaw, residue.CollateralIdleRaw, residue.MinimumCollateralDepositRaw = 1, 1, 1
	if got, ok := decideLeverageTarget(residue, SelectorResult{Action: "KEEP"}, markets, p, twentyBPSStepCost); !ok || got.outage() {
		t.Fatalf("residue froze the level decision: %+v %v", got, ok)
	}
	for name, c := range map[string]struct {
		result SelectorResult
		mutate func(*Snapshot)
	}{
		"selector enter":   {SelectorResult{Action: "ENTER"}, func(*Snapshot) {}},
		"nonterminal":      {SelectorResult{Action: "KEEP"}, func(s *Snapshot) { s.Nonterminal = Signed }},
		"withdrawal":       {SelectorResult{Action: "KEEP"}, func(s *Snapshot) { s.WithdrawalDemandRaw = 1 }},
		"idle squads cash": {SelectorResult{Action: "KEEP"}, func(s *Snapshot) { s.SquadsIdleRaw = 1 }},
		"maple":            {SelectorResult{Action: "KEEP"}, func(s *Snapshot) { s.RouteLane, s.StrategyKey = MapleSyrupUSDCLaneID, MapleSyrupUSDCLaneID }},
	} {
		c2 := s
		c.mutate(&c2)
		if _, ok := decideLeverageTarget(c2, c.result, markets, p, twentyBPSStepCost); ok {
			t.Fatalf("%s: leverage decision ran", name)
		}
	}
}

// Live 2026-09-28: position at 1x (borrowing blocked), stored target 1.5x.
// When spread(1.5x) was unavailable (our borrow did not fit the pool's free
// liquidity) the rule fell back to "stay at 1x" and stored 1x; the next
// sample stored 1.5x again. Now: unavailable spread -> no decision, no write;
// available again -> the stored 1.5x stands (no flip).
func TestUnavailableSpreadNeverFlipsTheStoredTarget(t *testing.T) {
	t.Parallel()
	keep := SelectorResult{Action: "KEEP"}
	p := DefaultSelectorPolicy()
	s := leverageSnapshot(1) // debt-free, ~$1,000 equity
	s.LeverageTargetLevel = 1.5
	good := leverageMarket(s.RouteLane, 0.12, math.Log1p(0.06))
	full := good
	full.DebtSupplyRaw, full.DebtBorrowRaw = 1_000_000_000, 900_000_000 // $100 free < $500 borrow
	if _, ok := leverageSpread(full, 1.5, 1_000_000_000, nil); ok {
		t.Fatal("fixture: spread(1.5x) should be unavailable")
	}
	if d, _ := decideLeverageTarget(s, keep, []LaneEconomics{full}, p, twentyBPSStepCost); d.Next != s.LeverageTargetLevel || d.BorrowRaw != 0 {
		t.Fatalf("unavailable spread still decided: %+v", d)
	}
	// Stored 1.5x with a 2+ pt spread: the only change is the next step up.
	d, ok := decideLeverageTarget(s, keep, []LaneEconomics{good}, p, twentyBPSStepCost)
	if !ok || (d.Next != 1.5 && d.Next != 1.75) || (d.Next == 1.5 && levelChanges(d, s.LeverageTargetLevel)) {
		t.Fatalf("available again: %+v ok=%t changes=%t", d, ok, levelChanges(d, s.LeverageTargetLevel))
	}
	// A dip in gain below the up gate does not undo a stored target either:
	// the rule steps from the stored level (down from 1.5x only below 0).
	weak := leverageMarket(s.RouteLane, 0.10, math.Log1p(0.095))
	if d, ok := decideLeverageTarget(s, keep, []LaneEconomics{weak}, p, twentyBPSStepCost); !ok || levelChanges(d, 1.5) {
		t.Fatalf("weak spread flipped the stored 1.5x: %+v", d)
	}
	neg := leverageMarket(s.RouteLane, 0.05, math.Log1p(0.06))
	if d, ok := decideLeverageTarget(s, keep, []LaneEconomics{neg}, p, twentyBPSStepCost); !ok || d.Next != 1 || !levelChanges(d, 1.5) {
		t.Fatalf("negative spread did not step down: %+v", d)
	}
	if levelChanges(leverageDecision{Lane: autoAUTOPYUSD.Lane, Current: 1.75, Next: 1.75}, 1.75) {
		t.Fatal("stored 1.75x rewritten")
	}
	if !levelChanges(leverageDecision{Current: 1, Next: 1.5}, 0) {
		t.Fatal("no stored target: first decision not written")
	}
}

func leverageTestPriceBytes() []byte { return []byte{0, 0, 0, 0, 0, 0, 0, 16, 0, 0, 0, 0, 0, 0, 0, 0} }

func armLeverageCapacityFixture(s *Snapshot) {
	s.LeverageBorrowRaw, s.LeverageDesiredRaw = [len(leverageLadder)]uint64{}, [len(leverageLadder)]uint64{}
	s.BorrowCapacityKnown = true
	s.AdditionalDebtRoomRaw = 1_000_000_000
	s.BorrowUtilizationHeadroomRaw = math.MaxUint64
	s.BorrowDebtDecimals = 6
	copy(s.BorrowDebtPriceSF[:], leverageTestPriceBytes())
	s.BorrowUSDCPriceSF = s.BorrowDebtPriceSF
	position := leverageTestPosition(uint64(s.PositionCollateralRaw), uint64(s.PositionDebtRaw))
	for i, r := range leverageLadder[1:] {
		if r.level > laneMaxLevel(s.RouteLane) {
			break
		}
		if n, err := position.leverageUpBorrowRaw(r.level); err == nil {
			s.LeverageBorrowRaw[i+1], _ = leverageUpCapFee(position, laneCeilingLTVBPS(s.RouteLane), n, func(uint64) (uint64, error) { return 0, nil })
		}
	}
	s.LeverageDesiredRaw = s.LeverageBorrowRaw
	s.LeverageUpApproved = true
}

// levelChanges reports whether storing d changes the stored level in effect
// (a stored level above the lane's cap counts as the cap).
func levelChanges(d leverageDecision, stored float64) bool {
	if stored > 0 {
		stored = min(stored, laneMaxLevel(d.Lane))
	}
	return d.Next != stored
}
