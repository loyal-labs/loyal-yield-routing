package backyard

import "testing"

func TestPilotPlannerSizesOneTrancheAndPreservesExitPriority(t *testing.T) {
	t.Parallel()
	for _, lane := range basicLaneIDs() {
		s := base()
		s.RouteLane = lane
		s.StrategyKey = lane
		tranche := int64(strategyTwoBridgeLegCapRaw)
		s.SelectorEntryEquityRaw = tranche
		s.VoltrIdleRaw = tranche
		s.CapacityRaw = tranche
		s.PolicyLimitRaw = tranche
		s.MaxTargetLTVEntryRaw = tranche
		d := Decide(s)
		if d.Action != VoltrAllocateToSquads || d.AmountRaw != 200_000_000_000 || d.Validate() != nil {
			t.Fatalf("pilot %s sizing: %+v", lane, d)
		}
		s.CapacityRaw = 3_000_000
		if stale := Decide(s); stale.Action != Hold {
			t.Fatalf("shrinking capacity reused larger quote: %+v", stale)
		}
		s.SelectorEntryEquityRaw = 3_000_000
		if sized := Decide(s); sized.AmountRaw != 3_000_000 {
			t.Fatalf("capacity ignored: %+v", sized)
		}
		s.VoltrIdleRaw = 0
		s.SquadsIdleRaw = 10_000_000
		s.Unwind = true
		d = Decide(s)
		if d.Action != StageSquadsToVoltr || d.AmountRaw != 10_000_000 {
			t.Fatalf("pilot full return clamped: %+v", d)
		}
		s.Unwind = false
		s.CapacityRaw = 1
		if d = Decide(s); d.Action != StageSquadsToVoltr || d.AmountRaw != 10_000_000 {
			t.Fatalf("capacity race strands cash: %+v", d)
		}
	}
}

func TestPilotWithdrawalPreservesFullExitAmount(t *testing.T) {
	t.Parallel()
	d := Decision{Action: DeleverRouteStep, StrategyKey: SelectedRouteID, Reason: "withdrawal_withdraw_collateral"}
	p := KaminoPosition{HasPosition: true, CollateralDepositedRaw: 12_000_000, RedeemablePrimeRaw: 15_000_000}
	leg, receipt, liquidity, err := selectKaminoLeg(d, p)
	if err != nil || leg != kaminoLegWithdraw || receipt != 12_000_000 || liquidity != 15_000_000 {
		t.Fatalf("pilot exit clipped by canary: %d %d %d %v", leg, receipt, liquidity, err)
	}
}

// An admitted, still-valid selector entry allocates before an age-only NAV
// report: the allocation carries its own adaptor report, and reporting first
// outlives the entry's 30-second quote window (2026-09-24 canary, ASK-2297).
// Drift and withdrawal demand keep their priority, and without an admitted
// entry the age-only report still runs.
func TestAdmittedEntryAllocatesBeforeAgeOnlyReport(t *testing.T) {
	t.Parallel()
	for _, lane := range basicLaneIDs() {
		s := base()
		s.RouteLane, s.StrategyKey = lane, lane
		s.SelectorEntryEquityRaw, s.VoltrIdleRaw = 200_000_000, 256_387_976
		s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw = 1_000_000_000, 1_000_000_000, 1_000_000_000
		s.LastReportAgeSeconds = 3605
		if d := Decide(s); d.Action != VoltrAllocateToSquads || d.AmountRaw != 200_000_000 {
			t.Fatalf("%s: admitted entry did not allocate before age-only report: %+v", lane, d)
		}
		noEntry := s
		noEntry.SelectorEntryEquityRaw = 0
		if d := Decide(noEntry); d.Action != ReportNAV {
			t.Fatalf("%s: age-only report skipped without an admitted entry: %+v", lane, d)
		}
		paused := s
		paused.SelectorEntryPaused = true
		if d := Decide(paused); d.Action != ReportNAV {
			t.Fatalf("%s: paused entry skipped the age-only report: %+v", lane, d)
		}
		withdrawal := s
		withdrawal.WithdrawalDemandRaw = 1
		if d := Decide(withdrawal); d.Action == VoltrAllocateToSquads {
			t.Fatalf("%s: allocation preempted withdrawal handling: %+v", lane, d)
		}
		mutated := s
		mutated.PostMutationNAVRequired = true
		if d := Decide(mutated); d.Action == VoltrAllocateToSquads {
			t.Fatalf("%s: allocation preempted a required post-mutation report: %+v", lane, d)
		}
	}
}
