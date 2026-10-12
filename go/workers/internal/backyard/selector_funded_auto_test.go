package backyard

import (
	"testing"
	"time"
)

// fundedAutoFixture builds a pilot-sized Maple→AUTO candidate with real bound
// PYUSD debt and AUTO collateral price evidence: the exact priced quote shape
// entry admission and the priced persistence window consume. The quote is
// fresh at the fixture's slot and re-freshed by advanceSelectorFixture.
func fundedAutoFixture(t *testing.T, debtPrice, collateralPrice BudgetPrice) SelectorInput {
	t.Helper()
	in := selectorFixture()
	advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now))
	in.Snapshot.Slot = 42
	in.Snapshot.VoltrIdleRaw, in.Snapshot.TotalVaultNAVRaw = 100_000_000, 100_000_000
	market := in.Markets[0]
	market.Lane = testAutoLane
	market.EntryCapacity = Capacity{Known: true, Raw: 10_000_000}
	in.Markets = []LaneEconomics{market}
	debt, collateral := debtPrice, collateralPrice
	quote := MoveQuote{
		MinimumIdleRaw: uint64(in.Snapshot.TotalVaultNAVRaw), BorrowReceiveRaw: 5_000_000, BorrowFeeRaw: 10_000,
		DebtPrice: &debt, CollateralAssetPrice: &collateral, RedepositCollateralRaw: 5_000_000,
		SourceLane: in.Snapshot.RouteLane, DestinationLane: testAutoLane, ObservationID: in.Snapshot.ObservationID,
		EquityRaw: 10_000_000, CostRaw: 10_000, ObservedAt: in.Now,
		EvidenceID: sha256Bytes([]byte("auto-priced-quote")), SampleSlot: in.Snapshot.Slot, ValidThroughSlot: in.Snapshot.Slot + budgetMaxObservationLagSlots,
	}
	in.Quotes = []MoveQuote{quote}
	return in
}

// fundedAutoSourceFixture builds the funded follow-up state: an AUTO position
// is the production route, so the selector must keep its economics, hold the
// same-lane baseline, and stay able to unwind.
func fundedAutoSourceFixture(t *testing.T, debtPrice, collateralPrice BudgetPrice) SelectorInput {
	t.Helper()
	in := fundedAutoFixture(t, debtPrice, collateralPrice)
	s := in.Snapshot
	s.RouteLane, s.StrategyKey = testAutoLane, testAutoLane
	s.HasPosition = true
	s.PositionCollateralRaw, s.PositionCollateralValueRaw = 4_000_000, 4_000_000
	s.PositionDebtRaw, s.PositionDebtValueRaw = 1_000_000, 1_000_000
	s.StrategyNAVRaw = 3_000_000
	s.VoltrIdleRaw = 0
	in.Snapshot = s
	// The destination candidate: a profitable installed lane quoted from the
	// AUTO source at the pilot tranche, carrying the exact bounded exit for
	// the live position.
	source := in.Markets[0]
	destination := source
	destination.Lane = "OnRe/ONyc/USDC"
	destination.NativeAPY = .5
	in.Markets = []LaneEconomics{source, destination}
	quote := MoveQuote{
		MinimumIdleRaw: 100_000_000, BorrowReceiveRaw: 5_000_000,
		SourceExit: &selectorExitBound{MaxCollateralRaw: 4_000_000, MaxDebtRaw: 1_000_000},
		SourceLane: testAutoLane, DestinationLane: destination.Lane, ObservationID: s.ObservationID,
		EquityRaw: 10_000_000, CostRaw: 10_000, ObservedAt: in.Now,
		EvidenceID: sha256Bytes([]byte("auto-source-unwind")), SampleSlot: s.Slot, ValidThroughSlot: s.Slot + budgetMaxObservationLagSlots,
	}
	in.Quotes = []MoveQuote{quote}
	return in
}

// The candidate AUTO lane must select through ordinary priced persistence: no
// operator canary, no parity assumption. The first profitable sample opens the
// window, a later sample inside MaxSampleGap at Persistence selects, and a gap
// beyond MaxSampleGap restarts the hysteresis. AUTO is a registry lane, so
// there is no separate embedded closure to compare against.
func TestAutoCandidateOrdinarySelectionAcrossSamples(t *testing.T) {
	t.Parallel()
	_, debtPrice, _ := autoDebtPriceFixture(t, 1_000_000)
	collateralPrice := autoCollateralPriceFixture(t, 1_000_000)
	in := fundedAutoFixture(t, debtPrice, collateralPrice)
	result := SelectOpportunity(in, SelectorState{})
	if result.Action != "KEEP" || result.Reason != "advantage_not_yet_persistent" {
		t.Fatal("first profitable sample must hold for persistence", result)
	}
	window, ok := result.State.Advantages[testAutoLane]
	if !ok || !window.Since.Equal(in.Now) || !window.LastSample.Equal(in.Now) {
		t.Fatal("priced sample did not open the window", result.State)
	}
	for _, c := range result.Candidates {
		if c.Lane == testAutoLane && (!c.CostsKnown || c.BlockedReason != "" || c.BenefitRaw <= 0) {
			t.Fatal("priced candidate lost its economics", c)
		}
	}
	advanceSelectorFixture(&in, time.Minute)
	result = SelectOpportunity(in, result.State)
	if result.Action != "ENTER" || result.Reason != "persistent_net_benefit" || result.DestinationLane != testAutoLane || result.SelectedQuote == nil {
		t.Fatal("priced persistence did not select the candidate", result)
	}
	// Hysteresis: a gap beyond MaxSampleGap restarts the window instead of
	// carrying stale persistence into an entry.
	advanceSelectorFixture(&in, in.Policy.MaxSampleGap+time.Minute)
	result = SelectOpportunity(in, result.State)
	if result.Action != "KEEP" || result.Reason != "advantage_not_yet_persistent" {
		t.Fatal("stale window survived a MaxSampleGap breach", result)
	}
	if window, ok := result.State.Advantages[testAutoLane]; !ok || !window.Since.Equal(in.Now) {
		t.Fatal("restarted window lost its fresh start", result.State)
	}
}

// Full, unknown, deferred and unpriced candidate markets must hold without
// accumulating any persistence: only a complete priced quote samples.
func TestAutoCandidateFullAndUnavailableCapacityHoldWithoutPersistence(t *testing.T) {
	t.Parallel()
	_, debtPrice, _ := autoDebtPriceFixture(t, 1_000_000)
	collateralPrice := autoCollateralPriceFixture(t, 1_000_000)
	cases := []struct {
		name    string
		mutate  func(*SelectorInput)
		blocked string
	}{
		{"full", func(in *SelectorInput) { in.Markets[0].EntryCapacity = Capacity{Known: true, Raw: 0} }, "entry_closed"},
		{"unknown", func(in *SelectorInput) { in.Markets[0].EntryCapacity = Capacity{} }, "pair_capacity_unknown"},
		{"deferred", func(in *SelectorInput) { in.Markets[0].EntryBlockedReason = "lane_entry_deferred" }, "lane_entry_deferred"},
		{"unpriced", func(in *SelectorInput) { in.Quotes[0].DebtPrice = nil }, "bounded_borrow_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := fundedAutoFixture(t, debtPrice, collateralPrice)
			tc.mutate(&in)
			result := SelectOpportunity(in, SelectorState{})
			if result.Action != "KEEP" || result.Reason != "no_worthwhile_executable_move" {
				t.Fatal("held market reached selection", result)
			}
			var candidate *CandidateForecast
			for i := range result.Candidates {
				if result.Candidates[i].Lane == testAutoLane {
					candidate = &result.Candidates[i]
				}
			}
			if candidate == nil || candidate.BlockedReason != tc.blocked {
				t.Fatal("hold not labeled on the candidate", candidate)
			}
			if len(result.State.Advantages) != 0 {
				t.Fatal("held market accumulated persistence", result.State)
			}
		})
	}
	in := fundedAutoFixture(t, debtPrice, collateralPrice)
	// The installed state: the embedded manifest's complete installed binding
	// opens the same persistence window the reviewed initializer fixture does.
	installedResult := SelectOpportunity(in, SelectorState{})
	if installedResult.Action != "KEEP" || installedResult.Reason != "advantage_not_yet_persistent" || len(installedResult.State.Advantages) != 1 {
		t.Fatal("installed binding did not open the candidate persistence window", installedResult)
	}
	if window, ok := installedResult.State.Advantages[testAutoLane]; !ok || !window.Since.Equal(in.Now) {
		t.Fatal("installed persistence window drifted", installedResult.State)
	}
}

// The funding authority is the active registry: Maple and AUTO fund, an
// unknown lane never does.
func TestSelectorEntryFundingLaneAuthority(t *testing.T) {
	t.Parallel()
	manifest := embeddedTestManifest(t)
	cases := []struct {
		name        string
		manifest    RouteManifest
		maple       bool
		autoFund    bool
		autoInitial bool
	}{
		{"embedded", manifest, true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := earnActiveLane(SelectedRouteID); got != tc.maple {
				t.Fatal("installed lane funding changed", got)
			}
			if got := earnActiveLane(testAutoLane); got != tc.autoFund {
				t.Fatal("candidate funding authority", got)
			}
			if got := earnInitializerLane(testAutoLane); got != tc.autoInitial {
				t.Fatal("candidate initializer authority", got)
			}
			if got := earnActiveLane("Unreviewed/Lane"); got {
				t.Fatal("unknown lane admitted", got)
			}
		})
	}
}
