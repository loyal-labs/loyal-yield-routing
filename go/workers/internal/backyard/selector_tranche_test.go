package backyard

import (
	"strings"
	"testing"
	"time"
)

// trancheMarket is fresh evidence for one lane: a flat borrow curve at
// borrowAPR and a native yield.
func trancheMarket(lane string, yield, borrowAPR float64, now time.Time) LaneEconomics {
	m := leverageMarket(lane, yield, borrowAPR)
	m.EvidenceID, m.ObservedAt, m.NativeObservedAt = "evidence:"+lane, now, now
	return m
}

// trancheAUTOSource is a settled $100k AUTO position at 1x with borrowing
// blocked: the live Oct 11 shape, a lone held lane with no cash in flight.
func trancheAUTOSource() Snapshot {
	s := base()
	s.Slot = 455_000_000
	s.ObservationID = "auto-source"
	s.RouteLane, s.StrategyKey, s.HeldLanes = autoAUTOPYUSD.Lane, autoAUTOPYUSD.Lane, autoAUTOPYUSD.Lane
	s.HasPosition, s.ObligationPresent, s.ObligationPresenceKnown = true, true, true
	s.PositionCollateralRaw, s.PositionCollateralValueRaw = 100_000_000_000, 100_000_000_000
	s.StrategyNAVRaw, s.TotalVaultNAVRaw, s.PriorReportedNAVRaw = 100_000_000_000, 100_000_000_000, 100_000_000_000
	s.LeverageTargetLevel, s.BorrowUtilizationBlocked = 1.75, true
	return s
}

// trancheQuote is an executable tranche quote of E into destination.
func trancheQuote(s Snapshot, destination string, equity int64, now time.Time) MoveQuote {
	expected := int64(10_000_000)
	return MoveQuote{Tranche: true, SourceLane: s.RouteLane, DestinationLane: destination, EquityRaw: equity, MinimumIdleRaw: uint64(equity),
		CostRaw: 30_000_000, ExpectedCostRaw: &expected, Unlevered: true, ObservationID: s.ObservationID, ObservedAt: now,
		SampleSlot: s.Slot, ValidThroughSlot: s.Slot + 10, EvidenceID: strings.Repeat("e", 64)}
}

// A tranche moves E from AUTO into OnRe when OnRe has $3k of room: AUTO
// frees exactly that much with its debt-preserving partial legs and stays
// held, the working lane hands over to OnRe once AUTO's custodies are clear,
// OnRe enters with the reserved idle cash, and leverage-up then runs on
// OnRe toward OnRe's own target.
func TestTrancheMovesEFromAUTOIntoOnReAndLeversOnRe(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 11, 2, 0, 0, 0, time.UTC)
	policy := DefaultSelectorPolicy()
	source := trancheAUTOSource()
	markets := func(now time.Time) []LaneEconomics {
		return []LaneEconomics{trancheMarket(autoAUTOPYUSD.Lane, 0.08, 0.09, now), trancheMarket(onreONycUSDC, 0.20, 0.05, now)}
	}
	room := int64(3_000_000_000)
	in := SelectorInput{Now: start, Snapshot: source, Markets: markets(start), Quotes: []MoveQuote{trancheQuote(source, onreONycUSDC, room, start)}, Policy: policy}

	// First sample: worth it, not yet persistent.
	first, tranche := applyTrancheSelection(in, SelectorResult{Action: "KEEP"}, nil, nil, 2, start)
	if tranche != nil || first.Action != "KEEP" {
		t.Fatalf("a first sample started a tranche: %+v", first)
	}
	var onre TrancheCandidate
	for _, c := range first.Tranches {
		if c.Destination == onreONycUSDC {
			onre = c
		}
	}
	if onre.BlockedReason != "" || onre.EquityRaw != room || onre.BenefitRaw <= float64(policy.MinimumBenefitRaw) {
		t.Fatalf("OnRe tranche was not priced on the margin: %+v", onre)
	}
	// Samples keep it ahead for 30 minutes; then the tranche starts, sized to
	// OnRe's room.
	windows := first.State.Tranches
	var later time.Time
	var second SelectorResult
	for minute := 5; minute <= 35; minute += 5 {
		later = start.Add(time.Duration(minute) * time.Minute)
		in.Now, in.Markets, in.Quotes = later, markets(later), []MoveQuote{trancheQuote(source, onreONycUSDC, room, later)}
		second, tranche = applyTrancheSelection(in, SelectorResult{Action: "KEEP"}, windows, nil, 3, later)
		windows = second.State.Tranches
		if minute < 30 && tranche != nil {
			t.Fatalf("tranche started after %d minutes", minute)
		}
	}
	if tranche == nil || second.Action != "TRANCHE" || tranche.Source != autoAUTOPYUSD.Lane || tranche.Destination != onreONycUSDC || tranche.EquityRaw != room || tranche.TargetIdleRaw != room {
		t.Fatalf("persistent tranche did not start: %+v %+v", second, tranche)
	}

	// Source half: AUTO frees E through the partial withdrawal legs.
	freeing := source
	applyTranche(&freeing, tranche)
	d := Decide(freeing)
	if d.Action != DeleverRouteStep || d.Reason != partialReleaseReason || d.StrategyKey != autoAUTOPYUSD.Lane || d.AmountRaw < room {
		t.Fatalf("source half did not release a partial: %+v", d)
	}
	if trancheTransition(*tranche, freeing, later) != trancheStay {
		t.Fatal("handoff before the source freed E")
	}
	// The legs reconciled: E is in Voltr idle, AUTO keeps the rest and its custodies are clear.
	freed := freeing
	freed.PositionCollateralRaw -= d.AmountRaw
	freed.PositionCollateralValueRaw -= d.AmountRaw
	freed.StrategyNAVRaw -= d.AmountRaw
	freed.VoltrIdleRaw = d.AmountRaw
	freed.PriorReportedNAVRaw = freed.StrategyNAVRaw
	if got := Decide(freed); got.Action != Hold || got.Reason != "tranche_source_freed" {
		t.Fatalf("freed source did not wait for the handoff: %+v", got)
	}
	if !freed.HasPosition || freed.PositionCollateralRaw <= 0 {
		t.Fatal("AUTO did not stay held")
	}
	if trancheTransition(*tranche, freed, later) != trancheHandoff {
		t.Fatal("freed source did not hand over to OnRe")
	}
	handed := *tranche
	handed.SourceFreed = true

	// Destination half: OnRe is flat; the working lane is OnRe, AUTO is held.
	destination := base()
	destination.Slot, destination.ObservationID = source.Slot+100, "onre-flat"
	destination.RouteLane, destination.StrategyKey, destination.HeldLanes = onreONycUSDC, onreONycUSDC, autoAUTOPYUSD.Lane
	destination.ObligationPresent, destination.ObligationPresenceKnown = true, true
	destination.VoltrIdleRaw = freed.VoltrIdleRaw
	destination.StrategyNAVRaw, destination.OtherLanesNAVRaw = freed.StrategyNAVRaw, freed.StrategyNAVRaw
	destination.TotalVaultNAVRaw = freed.StrategyNAVRaw + freed.VoltrIdleRaw
	destination.PriorReportedNAVRaw = freed.StrategyNAVRaw
	destination.CapacityRaw, destination.PolicyLimitRaw, destination.MaxTargetLTVEntryRaw = room, room, room
	applyTranche(&destination, &handed)
	if destination.TrancheDestinationRaw != room || destination.TrancheTargetIdleRaw != 0 {
		t.Fatalf("destination view did not carry E: %+v", destination)
	}
	if !unwindComplete(destination) {
		t.Fatal("a flat destination beside a held lane is not flat for entry")
	}
	entryQuote := trancheQuote(destination, onreONycUSDC, room, later)
	entryQuote.Tranche, entryQuote.SourceLane = false, onreONycUSDC
	entry, started := applyTrancheSelection(SelectorInput{Now: later, Snapshot: destination, Markets: markets(later), Quotes: []MoveQuote{entryQuote}, Policy: policy}, SelectorResult{Action: "KEEP"}, nil, &handed, 4, later)
	if started != nil || entry.Action != "ENTER" || entry.DestinationLane != onreONycUSDC || entry.EquityRaw != room {
		t.Fatalf("destination half did not enter OnRe with E: %+v", entry)
	}
	destination.SelectorEntryEquityRaw = room
	if got := Decide(destination); got.Action != VoltrAllocateToSquads || got.AmountRaw != room || got.StrategyKey != onreONycUSDC {
		t.Fatalf("OnRe entry did not allocate the reserved E: %+v", got)
	}
	if trancheTransition(handed, destination, later) != trancheStay {
		t.Fatal("tranche completed before OnRe was funded")
	}

	// OnRe funded at 1x beside the held AUTO lane; its own target levers it.
	funded := leverageSnapshot(1)
	funded.RouteLane, funded.StrategyKey = onreONycUSDC, onreONycUSDC
	funded.HeldLanes = onreONycUSDC + "," + autoAUTOPYUSD.Lane
	funded.LeverageTargetLevel = 0
	funded.TotalVaultNAVRaw, funded.StrategyNAVRaw = 1_000_000_000, 1_000_000_000
	armLeverageCapacityFixture(&funded)
	armFeeAuthorityFixture(t, &funded)
	applyTranche(&funded, &handed)
	target, ok := decideLeverageTarget(funded, SelectorResult{Action: "KEEP"}, []LaneEconomics{leverageMarket(onreONycUSDC, 0.60, 0.05)}, policy, twentyBPSStepCost)
	if !ok || target.Lane != onreONycUSDC || target.Next <= 1 {
		t.Fatalf("OnRe got no up target: %+v", target)
	}
	funded.LeverageTargetLevel = target.Next
	armLeverageCapacityFixture(&funded)
	if got := Decide(funded); got.Action != OpenRouteStep || got.Reason != leverageUpReason || got.StrategyKey != onreONycUSDC {
		t.Fatalf("leverage-up did not run on OnRe: %+v", got)
	}
	if trancheTransition(handed, funded, later) != trancheStay {
		t.Fatal("tranche completed before OnRe's leverage settled")
	}
	funded.BorrowUtilizationBlocked = true
	if trancheTransition(handed, funded, later) != trancheComplete {
		t.Fatal("tranche did not complete once OnRe was funded and its leverage settled")
	}
}

// A destination whose reserve has no room is refused for an economic reason
// every sample: it never accrues persistence, and a window it had is dropped
// rather than carried.
func TestBlockedTrancheDestinationNeverAccruesPersistence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 11, 2, 0, 0, 0, time.UTC)
	source := trancheAUTOSource()
	key := trancheWindowKey(source.RouteLane, onreONycUSDC)
	previous := map[string]AdvantageWindow{key: {Since: now.Add(-time.Hour), LastSample: now.Add(-time.Minute)}}
	for i := 0; i < 4; i++ {
		at := now.Add(time.Duration(i) * 15 * time.Minute)
		in := SelectorInput{Now: at, Snapshot: source, Policy: DefaultSelectorPolicy(),
			Markets:         []LaneEconomics{trancheMarket(autoAUTOPYUSD.Lane, 0.08, 0.09, at), trancheMarket(onreONycUSDC, 0.20, 0.05, at)},
			trancheRefusals: map[string]string{key: "tranche_destination_room_closed"}}
		selection := selectTranche(in, previous)
		if _, kept := selection.Windows[key]; kept || selection.Chosen != nil {
			t.Fatalf("sample %d: a closed destination kept or started persistence: %+v", i, selection)
		}
		previous = selection.Windows
	}
	// An outage, by contrast, carries the window unchanged.
	in := SelectorInput{Now: now, Snapshot: source, Policy: DefaultSelectorPolicy(),
		Markets: []LaneEconomics{trancheMarket(autoAUTOPYUSD.Lane, 0.08, 0.09, now), trancheMarket(onreONycUSDC, 0.20, 0.05, now)}}
	carried := map[string]AdvantageWindow{key: {Since: now.Add(-time.Hour), LastSample: now.Add(-time.Minute)}}
	if selection := selectTranche(in, carried); selection.Windows[key] != carried[key] {
		t.Fatal("a quote outage dropped the window")
	}
}

// A tranche never empties a lane: E stays strictly below the partial
// planner's 90% exclusion, so the last tranche out of a levered lane is the
// full-exit path, whose first capital leg needs the operator's debt-clear
// confirmation.
func TestLastTrancheOutOfLeveredLaneRequiresDebtClearConfirmation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 11, 2, 0, 0, 0, time.UTC)
	source := leverageSnapshot(1.5)
	source.ObservationID, source.HeldLanes = "levered", autoAUTOPYUSD.Lane
	source.BorrowUtilizationBlocked = true
	source.TotalVaultNAVRaw, source.StrategyNAVRaw = 1_000_000_000, 1_000_000_000
	equity := trancheSourceEquityRaw(source)
	max := trancheSourceMaxRaw(source)
	if max <= 0 || max*10_000 >= equity*partialWithdrawalFullExitBPS {
		t.Fatalf("partial cap %d is not below 90%% of equity %d", max, equity)
	}
	in := SelectorInput{Now: now, Snapshot: source, Policy: DefaultSelectorPolicy(),
		Markets: []LaneEconomics{trancheMarket(autoAUTOPYUSD.Lane, 0.08, 0.09, now), trancheMarket(onreONycUSDC, 0.60, 0.05, now)},
		Quotes:  []MoveQuote{trancheQuote(source, onreONycUSDC, equity, now)}}
	for _, c := range selectTranche(in, nil).Candidates {
		if c.Destination == onreONycUSDC && c.BlockedReason != "tranche_source_partial_unproven" {
			t.Fatalf("a tranche emptying the levered lane was offered: %+v", c)
		}
	}
	// Emptying it is the full-exit unwind: its first leg needs confirmation.
	plan := debtClearPlan{Snapshot: source, Decision: Decision{Action: SwapCollateralToDebtStep, StrategyKey: source.RouteLane}}
	required, err := debtClearRequired(readyWorkerManifest(t), JupiterSwapRequest{Action: SwapCollateralToDebtStep}, ExpectedEffects{}, plan,
		debtClearRouteState{Unwind: &UnwindIntent{SourceLane: source.RouteLane}})
	if err != nil || !required {
		t.Fatalf("emptying a levered lane did not require debt-clear confirmation: %t %v", required, err)
	}
}

// At most one Prime lane is held: with a Prime lane held, no tranche ever
// enters a second one, and Prime-to-Prime keeps the full-exit path.
func TestSecondPrimeLaneIsNeverEnteredWhileOneIsHeld(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 11, 2, 0, 0, 0, time.UTC)
	source := trancheAUTOSource()
	source.HeldLanes = autoAUTOPYUSD.Lane + "," + primePRIMEPYUSD.Lane
	var markets []LaneEconomics
	var quotes []MoveQuote
	for _, lane := range []string{autoAUTOPYUSD.Lane, PhaseOneLaneID, primePRIMEUSDS.Lane, onreONycUSDC} {
		markets = append(markets, trancheMarket(lane, 0.60, 0.05, now))
		if lane != autoAUTOPYUSD.Lane {
			quotes = append(quotes, trancheQuote(source, lane, 3_000_000_000, now))
		}
	}
	selection := selectTranche(SelectorInput{Now: now, Snapshot: source, Policy: DefaultSelectorPolicy(), Markets: markets, Quotes: quotes}, nil)
	for _, c := range selection.Candidates {
		switch c.Destination {
		case PhaseOneLaneID, primePRIMEUSDS.Lane:
			if c.BlockedReason != "tranche_sibling_lane_already_held" {
				t.Fatalf("a second Prime lane was a tranche destination: %+v", c)
			}
		case onreONycUSDC:
			if c.BlockedReason != "" {
				t.Fatalf("a non-Prime destination was refused: %+v", c)
			}
		}
	}
	// From a held Prime lane, another Prime lane is the full-exit path only.
	prime := source
	prime.RouteLane, prime.StrategyKey = primePRIMEPYUSD.Lane, primePRIMEPYUSD.Lane
	markets = append(markets, trancheMarket(primePRIMEPYUSD.Lane, 0.05, 0.05, now))
	for _, c := range selectTranche(SelectorInput{Now: now, Snapshot: prime, Policy: DefaultSelectorPolicy(), Markets: markets}, nil).Candidates {
		if (c.Destination == PhaseOneLaneID || c.Destination == primePRIMEUSDS.Lane) && c.BlockedReason != "tranche_sibling_uses_full_exit" {
			t.Fatalf("Prime-to-Prime offered as a tranche: %+v", c)
		}
	}
	if (trancheIntent{Source: primePRIMEPYUSD.Lane, Destination: primePRIMEUSDS.Lane, EquityRaw: 1, TargetIdleRaw: 1, Generation: 1, ObservationID: "o", EvidenceID: strings.Repeat("e", 64), CreatedAt: now}).validate() == nil {
		t.Fatal("a Prime-to-Prime tranche intent validated")
	}
}

// Maple's reviewed venue fills ~33.6k syrupUSDC (~$40k): the live $100k 1x
// rung (Oct 11, public quote) paid $61.20k of entry swap loss there. The next
// rung is sized to what the venue filled, not a tenth: at ~$35k the same
// recipe priced $1.09k expected cost ($867 swap loss), the lane's real cost.
func TestLadderSizesTheNextRungToTheVenueDepth(t *testing.T) {
	t.Parallel()
	const size, loss = uint64(100_122_623_645), uint64(61_203_946_702)
	next := selectorNextLadderSize(size, loss)
	if next < 34_000_000_000 || next > 36_000_000_000 {
		t.Fatalf("depth-limited rung sized %d, want ~35k (the filled value)", next)
	}
	// A rung that filled cleanly keeps the tenth-size ladder.
	if got := selectorNextLadderSize(10_012_262_929, 246_761_413); got != 1_001_226_292 {
		t.Fatalf("clean rung changed the ladder: %d", got)
	}
	// A transient failure of a smaller rung never leaves an insufficient
	// larger rung as the lane's only quote: the lane is an availability gap.
	big := MoveQuote{EquityRaw: int64(size)}
	if quotes, reason := selectorLadderOutcome([]MoveQuote{big}, false, true, ""); quotes != nil || reason != "complete_entry_quote_unavailable" {
		t.Fatalf("insufficient rung published after a smaller rung's outage: %v %q", quotes, reason)
	}
	if quotes, _ := selectorLadderOutcome([]MoveQuote{big}, false, false, ""); len(quotes) != 1 {
		t.Fatal("a lane priced at every rung lost its evidence")
	}
}

// The partial-exit source cost repeats the fixed per-transaction cost once
// per release round and scales swap loss with E.
func TestTrancheSourcePartialCostScalesSwapLoss(t *testing.T) {
	t.Parallel()
	s := trancheAUTOSource()
	expected := int64(4_000_000)
	recipe := selectorRecipe{CostRaw: 10_000_000, ExpectedCostRaw: &expected, Costs: []ValuedTransactionCost{
		{TotalMicros: 1_000_000, ExecutionCost: &PilotExecutionCost{NetworkMicros: 1_000_000}},
		{TotalMicros: 9_000_000, ExecutionCost: &PilotExecutionCost{NetworkMicros: 1_000_000, SwapLossMicros: 8_000_000}},
	}}
	bound, exp, err := trancheSourcePartialCost(recipe, s, s.PositionCollateralValueRaw/10)
	if err != nil || bound != 2_000_000+800_000 || exp != 2_000_000+200_000 {
		t.Fatalf("partial cost %d/%d %v, want 2_800_000/2_200_000", bound, exp, err)
	}
}
