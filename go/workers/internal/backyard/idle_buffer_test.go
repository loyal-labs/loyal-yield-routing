package backyard

import (
	"math"
	"testing"
	"time"
)

func TestCashReserveSizesEntryAndLeavesWithdrawalsUsable(t *testing.T) {
	for _, c := range []struct{ nav, reserve int64 }{
		{3_000_000_000, 3_000_000_000}, {40_000_000_000, 5_000_000_000},
		{175_000_000_000, 17_500_000_000}, {300_000_000_000, 25_000_000_000},
	} {
		in := selectorFixture()
		in.Policy = productionRWAPolicy
		in.Policy.DiversifyNewCash = false // Isolate cash sizing from provider admission.
		in.Snapshot.TotalVaultNAVRaw, in.Snapshot.VoltrIdleRaw = c.nav, c.nav
		q := &in.Quotes[0]
		q.MinimumIdleRaw, q.EquityRaw = uint64(c.nav), min(c.nav-c.reserve, int64(strategyTwoBridgeLegCapRaw))
		q.BorrowReceiveRaw = uint64(q.EquityRaw / 2)
		*q = parBorrow(*q)
		if got := in.Policy.idleBufferRaw(c.nav); got != c.reserve {
			t.Fatalf("NAV %d reserve %d want %d", c.nav, got, c.reserve)
		}
		if in.Snapshot.net != nil {
			in.Snapshot.net = fixtureSnapshotCertificate(in.Snapshot)
		}
		first := SelectOpportunity(in, SelectorState{})
		if c.nav == c.reserve {
			if first.Action != SelectorKeep || first.Reason != "no_investable_equity" {
				t.Fatal("spent small vault's reserve", first)
			}
			continue
		}
		candidate := first.Candidates[0]
		if !candidate.CostsKnown || candidate.IdleRaw != c.nav-q.EquityRaw {
			t.Fatalf("reserve consumed by entry: %+v", candidate)
		}
		q.EquityRaw++
		if in.Snapshot.net != nil {
			in.Snapshot.net = fixtureSnapshotCertificate(in.Snapshot)
		}
		if got := SelectOpportunity(in, SelectorState{}); got.Candidates[0].CostsKnown {
			t.Fatal("quote spent reserved cash")
		}
		withdrawal := livePartialSnapshot()
		withdrawal.VoltrIdleRaw, withdrawal.WithdrawalDemandRaw = c.reserve, c.reserve-1
		if got := Decide(targeted(withdrawal)); got.Action == DeleverRouteStep {
			t.Fatal("withdrew from investments despite available cash", got)
		}
	}
	p := productionRWAPolicy
	if got := p.idleBufferRaw(math.MaxInt64); got != 25_000_000_000 {
		t.Fatal("large NAV overflowed reserve", got)
	}
}

func TestShortHoldingHorizonRefusesSlowPaybackBorrow(t *testing.T) {
	s := leverageSnapshot(1)
	s.TotalVaultNAVRaw, s.StrategyNAVRaw = 1_000_000_000, 1_000_000_000
	armFeeAuthorityFixture(t, &s)
	market := leverageMarket(s.RouteLane, .12, math.Log1p(.06))
	long := productionRWAPolicy
	long.Horizon = 30 * 24 * time.Hour
	old, _ := decideLeverageTarget(s, SelectorResult{Action: SelectorKeep}, []LaneEconomics{market}, long, twentyBPSStepCost, nil)
	short, _ := decideLeverageTarget(s, SelectorResult{Action: SelectorKeep}, []LaneEconomics{market}, productionRWAPolicy, twentyBPSStepCost, nil)
	if old.BorrowRaw == 0 || short.BorrowRaw != 0 || short.GainRaw >= 0 {
		t.Fatalf("slow-payback borrowing survived: 30 days %+v; production %+v", old, short)
	}
}
