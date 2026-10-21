package backyard

import (
	"testing"
	"time"
)

// obligationEntryStates are the entry-ready books each installed lane
// presents while bridge capital is waiting to become collateral, with the
// selector's entry into that lane.
func obligationEntryStates() []struct {
	name     string
	lane     string
	seed     func(Snapshot) Snapshot
	expected Action
} {
	return []struct {
		name     string
		lane     string
		seed     func(Snapshot) Snapshot
		expected Action
	}{
		{"selected allocates voltr idle", SelectedRouteID, func(s Snapshot) Snapshot { s.VoltrIdleRaw = 4; return s }, VoltrAllocateToSquads},
		{"selected swaps usdc for collateral", SelectedRouteID, func(s Snapshot) Snapshot { s.SquadsIdleRaw = 4; return s }, SwapStableToCollateralStep},
		{"selected deposits collateral", SelectedRouteID, func(s Snapshot) Snapshot { s.CollateralIdleRaw = 4_000; return s }, OpenRouteStep},
		{"non usdc swaps usdc for collateral", autoAUTOPYUSD.Lane, func(s Snapshot) Snapshot { s.SquadsIdleRaw = 4; return s }, SwapStableToCollateralStep},
		{"non usdc allocates voltr idle", autoAUTOPYUSD.Lane, func(s Snapshot) Snapshot { s.VoltrIdleRaw = 4; return s }, VoltrAllocateToSquads},
	}
}

// entryTargeted is s, entering lane on the selector's current quote.
func entryTargeted(t *testing.T, s Snapshot, lane string) Snapshot {
	s.RouteLane, s.StrategyKey, s.Slot = lane, lane, 42
	q := entryQuoteFixture(time.Now().UTC(), lane, 4)
	if lane == autoAUTOPYUSD.Lane {
		q = autoEntryQuoteFixture(time.Now().UTC(), 4)
		q.BorrowFeeRaw = 0
		q = parBorrow(q)
	}
	return targetedWith(s, targetInputs{Entry: &SelectorResult{Action: "ENTER", DestinationLane: lane, SelectedQuote: &q}})
}

func TestAbsentObligationHoldsEntryBeforeAnyConstruction(t *testing.T) {
	t.Parallel()
	for _, state := range obligationEntryStates() {
		t.Run(state.name, func(t *testing.T) {
			s := state.seed(base())
			s.Obligation = obligationPresent
			baseline := Decide(entryTargeted(t, s, state.lane))
			if baseline.Action != state.expected {
				t.Fatalf("entry fixture resolved %+v instead of %v", baseline, state.expected)
			}
			s.Obligation = obligationAbsent
			hold := Decide(entryTargeted(t, s, state.lane))
			if initializationSnapshotReady(entryTargeted(t, s, state.lane)) {
				// A quoted selector entry creates the obligation: the one
				// construction an absent obligation admits.
				if hold.Action != InitializeKaminoObligation || hold.Validate() != nil {
					t.Fatalf("absent obligation with a quoted entry produced %+v", hold)
				}
				return
			}
			if hold.Action != Hold || hold.Reason != holdStale || hold.AmountRaw != 0 || hold.IdempotencyKey == baseline.IdempotencyKey {
				t.Fatalf("absent obligation produced %+v", hold)
			}
			if hold.StrategyKey != state.lane || hold.Validate() != nil {
				t.Fatalf("absent obligation hold lost its lane or is undecidable: %+v", hold)
			}
		})
	}
}

func TestAbsentObligationKeepsAccountingLegsLive(t *testing.T) {
	t.Parallel()
	s := base()
	s.WithdrawalDemandRaw = 3
	s.VoltrIdleRaw = 3
	s.LastReportAgeSeconds = 60
	baseline := Decide(targeted(s))
	if baseline.Action != ReportNAV {
		t.Fatalf("accounting fixture resolved %+v", baseline)
	}
	s.Obligation = obligationAbsent
	if got := Decide(targeted(s)); got.Action != ReportNAV || got.Reason != baseline.Reason || got.AmountRaw != baseline.AmountRaw {
		t.Fatalf("absent obligation blocked accounting: %+v vs %+v", got, baseline)
	}
}

func flattenObligation(accounts []ConfirmedAccount) []ConfirmedAccount {
	flattened := append([]ConfirmedAccount(nil), accounts...)
	for index := range flattened {
		if flattened[index].Address == primePRIMEUSDC.Kamino.Obligation {
			flattened[index] = ConfirmedAccount{Address: primePRIMEUSDC.Kamino.Obligation}
		}
	}
	return flattened
}

func TestObligationPresenceIsObservedNotAssumed(t *testing.T) {
	t.Parallel()
	manifest := readyWorkerManifest(t)
	config, err := pinnedKaminoObservationConfig()
	if err != nil {
		t.Fatal(err)
	}
	observe := func(accounts []ConfirmedAccount) KaminoPosition {
		t.Helper()
		position, err := observeKaminoFromFixedAccounts(77, accounts, config)
		if err != nil {
			t.Fatal(err)
		}
		return position
	}
	batch := routeNAVFixture(t, 77)
	for _, address := range []string{primePRIMEUSDC.Kamino.CollateralReserve, primePRIMEUSDC.Kamino.DebtReserve} {
		putKey(t, accountAt(batch, address).Data[5112:5144], kaminoScopePrices)
	}
	nav, err := ComputeRouteNAV(77, batch, manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if nav.Obligation != obligationPresent || nav.PositionCollateralValue != 30 || nav.PositionDebtValue != 7 {
		t.Fatalf("present obligation NAV: %+v", nav)
	}
	position := observe(append(batch, clockFixture()))
	if position.Obligation != obligationPresent || !position.HasPosition || position.CollateralDepositedRaw != 10 {
		t.Fatalf("present obligation position: %+v", position)
	}

	absentBatch := flattenObligation(batch)
	absentNAV, err := ComputeRouteNAV(77, absentBatch, manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if absentNAV.Obligation == obligationPresent || absentNAV.PositionCollateralValue != 0 || absentNAV.PositionDebtValue != 0 {
		t.Fatalf("absent obligation NAV: %+v", absentNAV)
	}
	if absentNAV.StrategyNAVRaw != nav.StrategyNAVRaw-nav.PositionCollateralValue+nav.PositionDebtValue {
		t.Fatalf("absent obligation NAV kept position value: %+v", absentNAV)
	}
	absentPosition := observe(append(absentBatch, clockFixture()))
	if absentPosition.Obligation == obligationPresent || absentPosition.HasPosition || absentPosition.CollateralDepositedRaw != 0 || absentPosition.DebtRaw != 0 {
		t.Fatalf("absent obligation decoded into a flat position: %+v", absentPosition)
	}
}
