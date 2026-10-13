package backyard

import (
	"strings"
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
		EvidenceID: sha256Bytes([]byte("prime-pyusd-priced-quote")), SampleSlot: in.Snapshot.Slot, ValidThroughSlot: in.Snapshot.Slot + budgetMaxObservationLagSlots,
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

// The held-sample line lists every candidate with leverage, net APY, borrow
// state, borrow room and benefit in USDC; its change key ignores drifting
// numbers but not a reserve opening.
func TestSelectorSampleLineListsEveryCandidate(t *testing.T) {
	t.Parallel()
	room, closed, feed := uint64(12_345_670_000), uint64(0), .1084
	result := SelectorResult{Action: "KEEP", Reason: "no_worthwhile_executable_move", SourceLane: autoAUTOPYUSD.Lane, Candidates: []CandidateForecast{
		{Lane: SelectedRouteID, CostsKnown: true, Leverage: 1.5, NetAPY: .0512, BenefitRaw: 420_000, DebtRoomUSDCRaw: &room},
		{Lane: onreONycUSDC, BlockedReason: "complete_entry_quote_unavailable"},
		{Lane: autoAUTOPYUSD.Lane, CostsKnown: true, Leverage: 1, NetAPY: .07, BorrowBlocked: true, BenefitRaw: -1, DebtRoomUSDCRaw: &closed, NativeAPY: .0885, FeedNativeAPY: &feed},
		{Lane: PhaseOneLaneID, BlockedReason: "entry_closed"},
		{Lane: primePRIMEPYUSD.Lane, BlockedReason: "pair_capacity_unknown"},
		{Lane: primePRIMEUSDS.Lane, BlockedReason: "economic_evidence_unavailable"},
	}}
	key, line := selectorSampleLine(result)
	for _, want := range []string{
		"Maple/syrupUSDC/USDC(lev=1.50x net=5.12% native=0.00% feed=unavailable borrow=open room=$12345.67 benefit=$0.42 reason=-)",
		// The realized native APY the forecast used, the feed's, and the gap.
		"AUTO/AUTO/PYUSD(lev=1.00x net=7.00% native=8.85% feed=10.84% gap=-1.99 borrow=blocked room=$0.00 benefit=$-0.00 reason=-)",
		"OnRe/ONyc/USDC(lev=- net=- native=0.00% feed=unavailable borrow=- room=- benefit=- reason=complete_entry_quote_unavailable)",
		"Prime/PRIME/USDS(", "Prime/PRIME/PYUSD(", "Prime/PRIME/USDC(",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("line misses %q:\n%s", want, line)
		}
	}
	drift := result
	drift.Candidates = append([]CandidateForecast(nil), result.Candidates...)
	drift.Candidates[0].NetAPY, drift.Candidates[0].BenefitRaw = .06, 1
	if driftKey, _ := selectorSampleLine(drift); driftKey != key {
		t.Fatal("drifting numbers changed the change key")
	}
	opened := uint64(3_400_000_000)
	drift.Candidates[2].DebtRoomUSDCRaw = &opened
	if openKey, _ := selectorSampleLine(drift); openKey == key {
		t.Fatal("a reserve opening did not change the change key")
	}
}
