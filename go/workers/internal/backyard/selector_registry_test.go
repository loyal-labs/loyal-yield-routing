package backyard

import (
	"testing"
	"time"
)

// Persistence is sampled only from an executable quote. A lane whose debt
// reserve sits above its utilization block has no leveraged quote, so however
// attractive its market economics, it never accrues the leveraged window; a
// 1x quote the collector prices instead persists only in its own lane|1x
// window.
func TestBlockedDebtReserveNeverAccruesLeveragedPersistence(t *testing.T) {
	t.Parallel()
	in := selectorFixture()
	lane := in.Markets[0].Lane
	in.Markets[0].NativeAPY = .5 // a market-level forecast would clear the bar
	in.Quotes = nil              // borrowing blocked: no leveraged quote
	state := SelectorState{}
	for i := 0; i < 3; i++ {
		result := SelectOpportunity(in, state)
		if _, ok := result.State.Advantages[lane]; ok {
			t.Fatalf("sample %d: blocked lane accrued persistence: %+v", i, result.State)
		}
		state = result.State
		advanceSelectorFixture(&in, time.Minute)
	}
	unlevered := selectorFixture()
	unlevered.Markets[0].NativeAPY = .5
	q := &unlevered.Quotes[0]
	q.Unlevered, q.BorrowReceiveRaw, q.BorrowFeeRaw, q.BorrowValueRaw, q.ProceedsValueRaw = true, 0, 0, 0, 0
	result := SelectOpportunity(unlevered, SelectorState{})
	if _, ok := result.State.Advantages[lane]; ok {
		t.Fatal("1x quote accrued the leveraged window", result.State)
	}
	if _, ok := result.State.Advantages[unleveredAdvantageKey(lane)]; !ok {
		t.Fatal("profitable 1x quote did not open its own window", result.State)
	}
}

// primePYUSDQuote is an executable Prime/PRIME/PYUSD entry from idle Maple
// cash, its PYUSD borrow valued at par.
func primePYUSDQuote(in SelectorInput) MoveQuote {
	return parBorrow(MoveQuote{
		MinimumIdleRaw: uint64(in.Snapshot.TotalVaultNAVRaw), BorrowReceiveRaw: 5_000_000, BorrowFeeRaw: 10_000,
		SourceLane: in.Snapshot.RouteLane, DestinationLane: primePRIMEPYUSD.Lane, ObservationID: in.Snapshot.ObservationID,
		EquityRaw: 10_000_000, CostRaw: 10_000, ObservedAt: in.Now,
		EvidenceID: sha256Bytes([]byte("prime-pyusd-priced-quote")), SampleSlot: in.Snapshot.Slot, ValidThroughSlot: in.Snapshot.Slot + observationLagFloorSlots,
	})
}

// The owner's goal: with AUTO's PYUSD reserve full (no leveraged quote, its
// market blocked) and Prime/PRIME/PYUSD's reserve open, the selector ranks
// and, once persistent, enters Prime/PRIME/PYUSD.
func TestSelectorRanksPrimePYUSDWhenAUTOIsBlocked(t *testing.T) {
	t.Parallel()
	in := fundedAutoFixture(t)
	auto := in.Markets[0]
	auto.EntryCapacity, auto.EntryBlockedReason = Capacity{Known: true}, "no_borrow_room"
	prime := in.Markets[0]
	prime.Lane = primePRIMEPYUSD.Lane
	in.Markets = []LaneEconomics{auto, prime}
	in.Quotes = []MoveQuote{primePYUSDQuote(in)}
	first := SelectOpportunity(in, SelectorState{})
	var ranked, blocked bool
	for _, c := range first.Candidates {
		switch c.Lane {
		case primePRIMEPYUSD.Lane:
			ranked = c.CostsKnown && c.BlockedReason == "" && c.BenefitRaw > 0 && c.Leverage > 1 && !c.BorrowBlocked
		case autoAUTOPYUSD.Lane:
			blocked = c.BlockedReason == "no_borrow_room"
		}
	}
	if !ranked || !blocked || first.DestinationLane != primePRIMEPYUSD.Lane || first.Reason != "advantage_not_yet_persistent" {
		t.Fatalf("Prime/PRIME/PYUSD not ranked over blocked AUTO: %+v", first)
	}
	advanceSelectorFixture(&in, time.Minute)
	in.Quotes[0].ObservedAt = in.Now
	entered := SelectOpportunity(in, first.State)
	if entered.Action != "ENTER" || entered.DestinationLane != primePRIMEPYUSD.Lane || entered.SelectedQuote == nil {
		t.Fatalf("persistent Prime/PRIME/PYUSD advantage did not enter: %+v", entered)
	}
}
