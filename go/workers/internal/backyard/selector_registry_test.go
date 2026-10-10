package backyard

import (
	"math"
	"strings"
	"testing"
	"time"
)

// A move's worst-case CostRaw (swap minima, recipe limits) is an execution
// bound, not lost capital. Live 2026-10-10 an ONyc switch quoted CostRaw ~12%
// of equity while its expected expense was ~$340; scoring it must use the
// expected expense, so the bound alone never changes the candidate's benefit.
func TestOnReSwitchScoresItsExpectedCostNotItsBound(t *testing.T) {
	t.Parallel()
	const equity = int64(100_000_000_000) // $100k
	const expected = int64(340_000_000)   // $340
	score := func(costRaw int64) CandidateForecast {
		in := selectorFixture()
		in.Snapshot.VoltrIdleRaw, in.Snapshot.TotalVaultNAVRaw = equity, equity
		q := &in.Quotes[0]
		q.MinimumIdleRaw, q.EquityRaw, q.BorrowReceiveRaw = uint64(equity), equity, uint64(equity/2)
		q.CostRaw = costRaw
		cost := expected
		q.ExpectedCostRaw = &cost
		for _, c := range SelectOpportunity(in, SelectorState{}).Candidates {
			if c.Lane == onreONycUSDC && c.CostsKnown {
				return c
			}
		}
		t.Fatal("ONyc candidate not priced")
		return CandidateForecast{}
	}
	bounded := score(equity * 12 / 100)
	tight := score(expected)
	if bounded.InvestedRaw != equity-expected || tight.InvestedRaw != equity-expected {
		t.Fatalf("invested capital withheld the bound: %d %d", bounded.InvestedRaw, tight.InvestedRaw)
	}
	if bounded.BlockedReason != "" || math.Abs(bounded.BenefitRaw-tight.BenefitRaw) > 1e-6 || bounded.BenefitRaw <= 0 {
		t.Fatalf("worst-case bound changed the true benefit: %v vs %v (%s)", bounded.BenefitRaw, tight.BenefitRaw, bounded.BlockedReason)
	}
}

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
	q.Unlevered, q.BorrowReceiveRaw, q.BorrowFeeRaw = true, 0, 0
	result := SelectOpportunity(unlevered, SelectorState{})
	if _, ok := result.State.Advantages[lane]; ok {
		t.Fatal("1x quote accrued the leveraged window", result.State)
	}
	if _, ok := result.State.Advantages[unleveredAdvantageKey(lane)]; !ok {
		t.Fatal("profitable 1x quote did not open its own window", result.State)
	}
}

// primePYUSDQuote is an executable Prime/PRIME/PYUSD entry from idle Maple
// cash with bound PYUSD debt and PRIME collateral price evidence.
func primePYUSDQuote(t *testing.T, in SelectorInput, debtPrice BudgetPrice) MoveQuote {
	t.Helper()
	debt := debtPrice
	collateral := sfIntervalPrice(primePRIMEPYUSD.Kamino.CollateralMint, primePRIMEPYUSD.CollateralTokenProgram, 6, in.Snapshot.Slot)
	collateral.ValidThroughSlot = debt.ValidThroughSlot
	q := MoveQuote{
		MinimumIdleRaw: uint64(in.Snapshot.TotalVaultNAVRaw), BorrowReceiveRaw: 5_000_000, BorrowFeeRaw: 10_000,
		DebtPrice: &debt, CollateralAssetPrice: &collateral, RedepositCollateralRaw: 5_000_000,
		SourceLane: in.Snapshot.RouteLane, DestinationLane: primePRIMEPYUSD.Lane, ObservationID: in.Snapshot.ObservationID,
		EquityRaw: 10_000_000, CostRaw: 10_000, ObservedAt: in.Now,
		EvidenceID: sha256Bytes([]byte("prime-pyusd-priced-quote")), SampleSlot: in.Snapshot.Slot, ValidThroughSlot: debt.ValidThroughSlot,
	}
	asset, err := collateral.valueLower(10_000_000, primePRIMEPYUSD.Kamino.CollateralMint, primePRIMEPYUSD.CollateralTokenProgram, q.ValidThroughSlot)
	if err != nil || asset <= 0 {
		t.Fatal("collateral asset estimate", asset, err)
	}
	assetRaw := uint64(asset)
	q.CollateralAssetUSDCRaw = &assetRaw
	return q
}

// The owner's goal: with AUTO's PYUSD reserve full (no leveraged quote, its
// market blocked) and Prime/PRIME/PYUSD's reserve open, the selector ranks
// and, once persistent, enters Prime/PRIME/PYUSD.
func TestSelectorRanksPrimePYUSDWhenAUTOIsBlocked(t *testing.T) {
	t.Parallel()
	_, debtPrice, _ := autoDebtPriceFixture(t, 1_000_000)
	in := fundedAutoFixture(t, debtPrice, autoCollateralPriceFixture(t, 1_000_000))
	auto := in.Markets[0]
	auto.EntryCapacity, auto.EntryBlockedReason = Capacity{Known: true}, "no_borrow_room"
	prime := in.Markets[0]
	prime.Lane = primePRIMEPYUSD.Lane
	in.Markets = []LaneEconomics{auto, prime}
	in.Quotes = []MoveQuote{primePYUSDQuote(t, in, debtPrice)}
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
	room, closed := uint64(12_345_670_000), uint64(0)
	result := SelectorResult{Action: "KEEP", Reason: "no_worthwhile_executable_move", SourceLane: autoAUTOPYUSD.Lane, Candidates: []CandidateForecast{
		{Lane: SelectedRouteID, CostsKnown: true, Leverage: 1.5, NetAPY: .0512, BenefitRaw: 420_000, DebtRoomUSDCRaw: &room},
		{Lane: onreONycUSDC, BlockedReason: "complete_entry_quote_unavailable"},
		{Lane: autoAUTOPYUSD.Lane, CostsKnown: true, Leverage: 1, NetAPY: .07, BorrowBlocked: true, BenefitRaw: -1, DebtRoomUSDCRaw: &closed},
		{Lane: PhaseOneLaneID, BlockedReason: "entry_closed"},
		{Lane: primePRIMEPYUSD.Lane, BlockedReason: "pair_capacity_unknown"},
		{Lane: primePRIMEUSDS.Lane, BlockedReason: "economic_evidence_unavailable"},
	}}
	key, line := selectorSampleLine(result)
	for _, want := range []string{
		"Maple/syrupUSDC/USDC(lev=1.50x net=5.12% borrow=open room=$12345.67 benefit=$0.42 reason=-)",
		"AUTO/AUTO/PYUSD(lev=1.00x net=7.00% borrow=blocked room=$0.00 benefit=$-0.00 reason=-)",
		"OnRe/ONyc/USDC(lev=- net=- borrow=- room=- benefit=- reason=complete_entry_quote_unavailable)",
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
