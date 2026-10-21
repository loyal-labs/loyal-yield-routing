package backyard

import (
	"testing"
	"time"
)

// fundedAutoFixture builds a pilot-sized flat-vault AUTO candidate whose
// PYUSD borrow is valued at par: the priced quote shape entry admission and
// the priced persistence window consume. The quote is fresh at the fixture's
// slot and re-freshed by advanceSelectorFixture.
func fundedAutoFixture(t *testing.T) SelectorInput {
	t.Helper()
	in := selectorFixture()
	advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now))
	in.Snapshot.Slot = 42
	in.Snapshot.VoltrIdleRaw, in.Snapshot.TotalVaultNAVRaw = 100_000_000, 100_000_000
	market := in.Markets[0]
	market.Lane = testAutoLane
	market.EntryCapacity = Capacity{Known: true, Raw: 10_000_000}
	in.Markets = []LaneEconomics{market}
	quote := MoveQuote{
		MinimumIdleRaw: uint64(in.Snapshot.TotalVaultNAVRaw), BorrowReceiveRaw: 5_000_000, BorrowFeeRaw: 10_000,
		SourceLane: in.Snapshot.RouteLane, DestinationLane: testAutoLane, ObservationID: in.Snapshot.ObservationID,
		EquityRaw: 10_000_000, CostRaw: 10_000, ObservedAt: in.Now,
		EvidenceID: sha256Bytes([]byte("auto-priced-quote")), SampleSlot: in.Snapshot.Slot, ValidThroughSlot: in.Snapshot.Slot + observationLagFloorSlots,
	}
	in.Quotes = []MoveQuote{parBorrow(quote)}
	return in
}

// The candidate AUTO lane must select through ordinary priced persistence: no
// operator canary, no parity assumption. The first profitable sample opens the
// window, a later sample inside MaxSampleGap at Persistence selects, and a gap
// beyond MaxSampleGap restarts the hysteresis. AUTO is a registry lane, so
// there is no separate embedded closure to compare against.
func TestAutoCandidateOrdinarySelectionAcrossSamples(t *testing.T) {
	t.Parallel()
	in := fundedAutoFixture(t)
	result := SelectOpportunity(in, SelectorState{})
	if result.Action != "KEEP" || result.Reason != "advantage_not_yet_persistent" {
		t.Fatal("first profitable sample must hold for persistence", result)
	}
	window, ok := result.State.Advantages[advantageKey{lane: testAutoLane}]
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
	if window, ok := result.State.Advantages[advantageKey{lane: testAutoLane}]; !ok || !window.Since.Equal(in.Now) {
		t.Fatal("restarted window lost its fresh start", result.State)
	}
}

// Full, unknown, deferred and unpriced candidate markets must hold without
// accumulating any persistence: only a complete priced quote samples.
func TestAutoCandidateFullAndUnavailableCapacityHoldWithoutPersistence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(*SelectorInput)
		blocked string
	}{
		{"full", func(in *SelectorInput) { in.Markets[0].EntryCapacity = Capacity{Known: true, Raw: 0} }, "entry_closed"},
		{"unknown", func(in *SelectorInput) { in.Markets[0].EntryCapacity = Capacity{} }, "pair_capacity_unknown"},
		{"deferred", func(in *SelectorInput) { in.Markets[0].EntryBlockedReason = "lane_entry_deferred" }, "lane_entry_deferred"},
		{"unpriced", func(in *SelectorInput) { in.Quotes[0].BorrowValueRaw = 0 }, "bounded_borrow_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := fundedAutoFixture(t)
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
	in := fundedAutoFixture(t)
	// The installed state: the embedded manifest's complete installed binding
	// opens the same persistence window the reviewed initializer fixture does.
	installedResult := SelectOpportunity(in, SelectorState{})
	if installedResult.Action != "KEEP" || installedResult.Reason != "advantage_not_yet_persistent" || len(installedResult.State.Advantages) != 1 {
		t.Fatal("installed binding did not open the candidate persistence window", installedResult)
	}
	if window, ok := installedResult.State.Advantages[advantageKey{lane: testAutoLane}]; !ok || !window.Since.Equal(in.Now) {
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
			if got := earnActiveLane(MapleSyrupUSDCLaneID); got != tc.maple {
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
