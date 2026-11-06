package backyard

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestPolicyUpgradeRequiresFreshTradeEvidence(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		in := selectorFixture()
		in.Policy = DefaultSelectorPolicy()
		in.Policy.IdleBufferRaw = 5_000_000_000
		in.Snapshot.VoltrIdleRaw, in.Snapshot.TotalVaultNAVRaw = 100_000_000_000, 100_000_000_000
		resize := func() {
			q := &in.Quotes[0]
			q.MinimumIdleRaw = uint64(in.Snapshot.VoltrIdleRaw)
			q.EquityRaw = in.Snapshot.TotalVaultNAVRaw - in.Policy.idleBufferRaw(in.Snapshot.TotalVaultNAVRaw)
			if in.Policy.DiversifyNewCash {
				q.EquityRaw = min(q.EquityRaw, in.Snapshot.TotalVaultNAVRaw/3)
				in.Markets[0].EntryCapacity = Capacity{Known: true, Raw: q.EquityRaw}
			}
			q.BorrowReceiveRaw = uint64(q.EquityRaw / 2)
			*q = parBorrow(*q)
		}
		resize()
		var previous SelectorResult
		for minute := 0; minute <= 30; minute += 5 {
			if minute > 0 {
				advanceSelectorFixture(&in, 5*time.Minute)
			}
			if in.Snapshot.net != nil {
				in.Snapshot.net = fixtureSnapshotCertificate(in.Snapshot)
			}
			previous = SelectOpportunity(in, previous.State)
		}
		if previous.Action != SelectorEnter {
			t.Fatal("fixture did not accumulate the old policy's approval", previous)
		}
		if legacy {
			previous.State.Policy = nil
		}
		raw, err := json.Marshal(previous.State)
		var restarted SelectorState
		if err != nil || json.Unmarshal(raw, &restarted) != nil {
			t.Fatal("restart state", err)
		}
		in.Policy = productionRWAPolicy
		resize()
		advanceSelectorFixture(&in, time.Second)
		if in.Snapshot.net != nil {
			in.Snapshot.net = fixtureSnapshotCertificate(in.Snapshot)
		}
		result := SelectOpportunity(in, restarted)
		if result.Action != SelectorKeep || result.Reason != "advantage_not_yet_persistent" || result.SelectedQuote != nil {
			t.Fatalf("legacy=%v: upgrade reused old trade authority: %+v", legacy, result)
		}
		for minute := 5; minute <= 30; minute += 5 {
			advanceSelectorFixture(&in, 5*time.Minute)
			if in.Snapshot.net != nil {
				in.Snapshot.net = fixtureSnapshotCertificate(in.Snapshot)
			}
			result = SelectOpportunity(in, result.State)
			if (result.Action == SelectorEnter) != (minute == 30) {
				t.Fatalf("new policy persistence at minute %d: %+v", minute, result)
			}
		}
	}
}

func TestFullBorrowPoolDoesNotLiquidateExistingPosition(t *testing.T) {
	for _, level := range []float64{1.5, 1.75, 2, 2.5} {
		s := leverageSnapshot(level)
		s.RouteLane, s.StrategyKey = onreONycUSDC, onreONycUSDC
		s.BorrowUtilizationBlocked = true
		s.TotalVaultNAVRaw, s.StrategyNAVRaw = 1_000_000_000, 1_000_000_000
		s.AdditionalDebtRoomRaw = 0
		s.LeverageBorrowRaw, s.LeverageDesiredRaw = [len(leverageLadder)]uint64{}, [len(leverageLadder)]uint64{}
		armFeeAuthorityFixture(t, &s)
		market := leverageMarket(s.RouteLane, .10, math.Log1p(.06))
		market.DebtBorrowRaw = market.DebtSupplyRaw
		decision, ok := decideLeverageTarget(s, SelectorResult{Action: SelectorKeep}, []LaneEconomics{market}, productionRWAPolicy, nil, nil)
		if !ok || decision.Next < level || decision.BorrowRaw != 0 || decision.Down.MaxCostBPS != 0 {
			t.Fatalf("full pool reset a %.2fx position: %+v", level, decision)
		}
		s.LeverageTargetLevel, s.LeverageUpApproved, s.LeverageDown = decision.Next, decision.level().UpApproved, decision.Down
		if got := Decide(targeted(s)); got.Action != Hold {
			t.Fatalf("full pool triggered a position move: %+v", got)
		}
	}
}
