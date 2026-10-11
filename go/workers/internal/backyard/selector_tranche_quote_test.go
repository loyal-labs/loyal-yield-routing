package backyard

import (
	"context"
	"strings"
	"testing"
	"time"
)

func trancheCaptureFixture(s Snapshot) *selectorQuoteCapture {
	return &selectorQuoteCapture{source: selectorSourceQuote{Lane: s.RouteLane, ObservationID: s.ObservationID,
		Recipe: selectorRecipe{EvidenceID: strings.Repeat("a", 64)}}}
}

// Every way the collector prices nothing at all names its reason for the
// whole set, never a silent empty result.
func TestTrancheCollectionNamesEverySetLevelReason(t *testing.T) {
	t.Parallel()
	settled := trancheAUTOSource()
	planned := Observation{Snapshot: settled, planning: &routePlanningState{}}
	exitOnly := planned
	exitOnly.Snapshot.RouteLane = ethenaUSDePYUSD.Lane
	unsettled := planned
	unsettled.Snapshot.SquadsIdleRaw = 5
	inProgress := planned
	inProgress.planning = &routePlanningState{tranche: &trancheIntent{}}
	tiny := planned
	tiny.Snapshot.PositionCollateralRaw, tiny.Snapshot.PositionCollateralValueRaw = 1_000_000, 1_000_000
	foreignSource := trancheCaptureFixture(settled)
	foreignSource.source.Lane = onreONycUSDC
	staleSource := trancheCaptureFixture(settled)
	staleSource.source.ObservationID = "older"
	noEvidence := trancheCaptureFixture(settled)
	noEvidence.source.Recipe.EvidenceID = ""
	for _, tc := range []struct {
		name    string
		o       Observation
		capture *selectorQuoteCapture
		want    string
	}{
		{"no planning read", Observation{Snapshot: settled}, trancheCaptureFixture(settled), trancheReasonPlanningUnavailable},
		{"tranche in progress", inProgress, trancheCaptureFixture(settled), trancheReasonInProgress},
		{"exit-only source", exitOnly, trancheCaptureFixture(exitOnly.Snapshot), "tranche_source_full_exit_only"},
		{"cash in flight", unsettled, trancheCaptureFixture(settled), "tranche_finish_current_work_first"},
		{"no source quote", planned, nil, trancheReasonSourceQuote},
		{"another lane's source quote", planned, foreignSource, trancheReasonSourceQuote},
		{"another observation's source quote", planned, staleSource, trancheReasonSourceQuote},
		{"unidentified source quote", planned, noEvidence, trancheReasonSourceQuote},
		{"nothing the partial can free", tiny, trancheCaptureFixture(tiny.Snapshot), "tranche_source_partial_unproven"},
	} {
		got := collectTrancheQuotes(context.Background(), nil, nil, nil, RouteManifest{}, tc.o, nil, DefaultSelectorPolicy(), tc.capture)
		if got.SetReason != tc.want || len(got.Quotes) != 0 {
			t.Errorf("%s: set reason %q quotes %d, want %q", tc.name, got.SetReason, len(got.Quotes), tc.want)
		}
	}
}

// Every refused pair names its own reason: a flat destination whose priced
// rungs all exceed what the source can free, a rung whose move cannot be
// composed, a held USDC-debt destination that cannot take a top-up, and a
// held destination whose top-up recipe cannot be priced.
func TestTrancheCollectionNamesEveryPairReason(t *testing.T) {
	t.Parallel()
	s := trancheAUTOSource()
	s.HeldLanes = autoAUTOPYUSD.Lane + "," + onreONycUSDC + "," + SelectedRouteID
	o := Observation{Snapshot: s, planning: &routePlanningState{},
		heldFacts: []heldLaneFact{{Lane: onreONycUSDC, CollateralValueRaw: 5_000_000_000, DebtValueRaw: 1_000_000_000}, {Lane: SelectedRouteID, CollateralValueRaw: 5_000_000_000}}}
	capture := trancheCaptureFixture(s)
	maxE := uint64(trancheSourceMaxRaw(s))
	capture.destinations = []selectorDestinationQuote{
		{Lane: primePRIMEPYUSD.Lane, EquityRaw: maxE + 1},          // every rung above the partial cap
		{Lane: primePRIMEUSDS.Lane, EquityRaw: 1_000_000_000},      // composes against no view
		{Lane: primePRIMEPYUSD.Lane, EquityRaw: 2 * (maxE + 1)},    // a second rung, also too large
		{Lane: autoAUTOPYUSD.Lane, EquityRaw: 1_000_000_000},       // the source itself: never a destination
		{Lane: onreONycUSDC, EquityRaw: 1_000_000_000},             // held: priced as a top-up, not here
		{Lane: SelectedRouteID, EquityRaw: 1_000_000_000},          // held: priced as a top-up, not here
		{Lane: PhaseOneLaneID, EquityRaw: maxE + 1},                // sibling handling is the selector's
		{Lane: ethenaUSDePYUSD.Lane, EquityRaw: 1_000_000_000_000}, // above the partial cap
	}
	now := time.Now().UTC()
	markets := []LaneEconomics{trancheMarket(onreONycUSDC, 0.2, 0.05, now), trancheMarket(SelectedRouteID, 0.2, 0.05, now)}
	got := collectTrancheQuotes(context.Background(), nil, nil, nil, RouteManifest{}, o, markets, DefaultSelectorPolicy(), capture)
	want := map[string]string{
		primePRIMEPYUSD.Lane: trancheReasonNoRungFitsSource,
		primePRIMEUSDS.Lane:  trancheReasonMoveQuote,
		onreONycUSDC:         trancheReasonTopupUnsupported,
		SelectedRouteID:      trancheReasonDestinationQuote,
	}
	for lane, reason := range want {
		if r := got.Refusals[trancheWindowKey(s.RouteLane, lane)]; r != reason {
			t.Errorf("%s: reason %q, want %q", lane, r, reason)
		}
	}
	if r := got.Refusals[trancheWindowKey(s.RouteLane, autoAUTOPYUSD.Lane)]; r != "" || got.SetReason != "" || len(got.Quotes) != 0 {
		t.Fatalf("unexpected collection: %+v", got)
	}
}

// The collector's reasons reach the sample line, and keep their persistence
// meaning: an outage carries the pair's window, a closed room drops it, and
// a set-level outage labels every pair instead of a generic missing quote.
func TestTrancheReasonsReachCandidatesWithPersistenceSemantics(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 11, 2, 0, 0, 0, time.UTC)
	s := trancheAUTOSource()
	onre, maple := trancheWindowKey(s.RouteLane, onreONycUSDC), trancheWindowKey(s.RouteLane, SelectedRouteID)
	window := AdvantageWindow{Since: now.Add(-20 * time.Minute), LastSample: now.Add(-time.Minute)}
	previous := map[string]AdvantageWindow{onre: window, maple: window}
	markets := []LaneEconomics{trancheMarket(autoAUTOPYUSD.Lane, 0.08, 0.09, now), trancheMarket(onreONycUSDC, 0.2, 0.05, now), trancheMarket(SelectedRouteID, 0.2, 0.05, now)}
	in := SelectorInput{Now: now, Snapshot: s, Markets: markets, Policy: DefaultSelectorPolicy(),
		trancheRefusals: map[string]string{onre: trancheReasonDeadline, maple: trancheReasonRoomClosed}}
	selection := selectTranche(in, previous)
	reasons := map[string]string{}
	for _, c := range selection.Candidates {
		reasons[c.Destination] = c.BlockedReason
	}
	if reasons[onreONycUSDC] != trancheReasonDeadline || selection.Windows[onre] != window {
		t.Fatalf("an outage lost its reason or its window: %q %+v", reasons[onreONycUSDC], selection.Windows)
	}
	if _, kept := selection.Windows[maple]; reasons[SelectedRouteID] != trancheReasonRoomClosed || kept {
		t.Fatalf("a closed room kept its window: %q", reasons[SelectedRouteID])
	}
	// The whole-move ladder's own refusal of a flat destination is the pair's.
	in.trancheRefusals = nil
	in.Markets[2].EntryBlockedReason = "complete_entry_quote_unavailable"
	selection = selectTranche(in, previous)
	for _, c := range selection.Candidates {
		if c.Destination == SelectedRouteID && (c.BlockedReason != "complete_entry_quote_unavailable" || selection.Windows[maple] != window) {
			t.Fatalf("ladder refusal did not reach the pair: %+v", c)
		}
	}
	// A set-level outage labels every pair and carries every window.
	in.trancheSetReason = trancheReasonSourceQuote
	selection = selectTranche(in, previous)
	for _, c := range selection.Candidates {
		if c.BlockedReason != trancheReasonSourceQuote {
			t.Fatalf("set reason did not reach %s: %q", c.Destination, c.BlockedReason)
		}
	}
	if selection.Windows[onre] != window || selection.Windows[maple] != window {
		t.Fatal("a set-level outage dropped windows")
	}
}

// Prod Oct 11 02:00: a tranche quote of E=$91,084 matched the whole-move
// candidate's quote size for the same lane and held the whole selector with
// duplicate_move_quote. Tranche quotes never price a whole move.
func TestTrancheQuotesNeverMatchAWholeMove(t *testing.T) {
	t.Parallel()
	in := selectorFixture()
	whole := SelectOpportunity(in, SelectorState{})
	tranche := in.Quotes[0]
	tranche.Tranche, tranche.EvidenceID = true, strings.Repeat("t", 64)
	in.Quotes = append(in.Quotes, tranche)
	got := SelectOpportunity(in, SelectorState{})
	if got.Reason == "duplicate_move_quote" || got.Reason != whole.Reason || len(got.Candidates) != len(whole.Candidates) || got.Candidates[0] != whole.Candidates[0] {
		t.Fatalf("a tranche quote changed the whole-move selection: %+v vs %+v", got, whole)
	}
}

// A held destination's top-up that runs out of the collection window is an
// outage, named as the deadline.
func TestTrancheTopupPastTheDeadlineIsNamed(t *testing.T) {
	t.Parallel()
	s := trancheAUTOSource()
	s.HeldLanes = autoAUTOPYUSD.Lane + "," + SelectedRouteID
	o := Observation{Snapshot: s, planning: &routePlanningState{}, heldFacts: []heldLaneFact{{Lane: SelectedRouteID, CollateralValueRaw: 5_000_000_000}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	markets := []LaneEconomics{trancheMarket(SelectedRouteID, 0.2, 0.05, time.Now().UTC())}
	got := collectTrancheQuotes(ctx, nil, nil, nil, RouteManifest{}, o, markets, DefaultSelectorPolicy(), trancheCaptureFixture(s))
	if r := got.Refusals[trancheWindowKey(s.RouteLane, SelectedRouteID)]; r != trancheReasonDeadline || !selectorAvailabilityReasons[r] {
		t.Fatalf("deadline reason %q", r)
	}
}
