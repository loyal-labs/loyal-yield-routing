package backyardrwa

import (
	"context"
	"testing"
)

// obligationEntryStates are the entry-ready books each installed lane presents
// while bridge capital is waiting to become collateral.
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
		{"legacy allocates voltr idle", "", func(s Snapshot) Snapshot { s.VoltrIdleRaw = 4; return s }, VoltrAllocateToSquads},
		{"legacy swaps usdc for collateral", "", func(s Snapshot) Snapshot { s.SquadsIdleRaw = 4; return s }, SwapUSDCToPrimeStep},
		{"selected deposits collateral", SelectedRouteID, func(s Snapshot) Snapshot { s.CollateralIdleRaw = 4; return s }, OpenRouteStep},
		{"selected allocates voltr idle", SelectedRouteID, func(s Snapshot) Snapshot { s.VoltrIdleRaw = 4; return s }, VoltrAllocateToSquads},
		{"non usdc swaps usdc for collateral", "Ethena/USDe/PYUSD", func(s Snapshot) Snapshot { s.SquadsIdleRaw = 4; return s }, SwapStableToCollateralStep},
		{"non usdc allocates voltr idle", "Ethena/USDe/PYUSD", func(s Snapshot) Snapshot { s.VoltrIdleRaw = 4; return s }, VoltrAllocateToSquads},
	}
}

func TestAbsentObligationHoldsEntryBeforeAnyConstruction(t *testing.T) {
	for _, state := range obligationEntryStates() {
		t.Run(state.name, func(t *testing.T) {
			s := state.seed(base())
			s.RouteLane = state.lane
			baseline := Decide(s)
			if baseline.Action != state.expected {
				t.Fatalf("entry fixture resolved %v instead of %v", baseline.Action, state.expected)
			}
			s.ObligationPresenceKnown, s.ObligationPresent = true, true
			if got := Decide(s); got != baseline {
				t.Fatalf("present obligation changed the decision: %+v vs %+v", got, baseline)
			}
			s.ObligationPresent = false
			hold := Decide(s)
			if hold.Action != Hold || hold.Reason != obligationAbsentHoldReason || hold.AmountRaw != 0 {
				t.Fatalf("absent obligation produced %+v", hold)
			}
			if hold.Action == HoldManualRecovery || hold.IdempotencyKey == baseline.IdempotencyKey {
				t.Fatalf("absent obligation hold is not a plain distinct hold: %+v", hold)
			}
			if hold.StrategyKey != state.lane && !(state.lane == "" && hold.StrategyKey == RouteID) {
				t.Fatalf("absent obligation hold lost its lane: %+v", hold)
			}
		})
	}
}

func TestObligationHoldIsADecidableNonPolicyHold(t *testing.T) {
	for _, state := range obligationEntryStates() {
		t.Run(state.name, func(t *testing.T) {
			s := state.seed(base())
			s.RouteLane = state.lane
			s.ObligationPresenceKnown = true
			hold := Decide(s)
			if hold.Action != Hold || hold.Reason != obligationAbsentHoldReason {
				t.Fatalf("absent obligation produced %+v", hold)
			}
			if err := hold.Validate(); err != nil {
				t.Fatalf("obligation hold is not a decidable decision: %v", err)
			}
		})
	}
}

func TestAbsentObligationKeepsAccountingLegsLive(t *testing.T) {
	s := base()
	s.WithdrawalDemandRaw = 3
	s.VoltrIdleRaw = 3
	s.LastReportAgeSeconds = 60
	baseline := Decide(s)
	if baseline.Action != ReportNAV {
		t.Fatalf("accounting fixture resolved %+v", baseline)
	}
	s.ObligationPresenceKnown, s.ObligationPresent = true, false
	if got := Decide(s); got.Action != ReportNAV || got.Reason != baseline.Reason || got.AmountRaw != baseline.AmountRaw {
		t.Fatalf("absent obligation blocked accounting: %+v vs %+v", got, baseline)
	}
}

func flattenObligation(accounts []ConfirmedAccount) []ConfirmedAccount {
	flattened := append([]ConfirmedAccount(nil), accounts...)
	for index := range flattened {
		if flattened[index].Address == kaminoPrimeUSDCObligation {
			flattened[index] = ConfirmedAccount{Address: kaminoPrimeUSDCObligation}
		}
	}
	return flattened
}

func TestObligationPresenceIsObservedNotAssumed(t *testing.T) {
	manifest := readyWorkerManifest(t)
	config, err := pinnedKaminoObservationConfig()
	if err != nil {
		t.Fatal(err)
	}
	observe := func(accounts []ConfirmedAccount) KaminoPosition {
		t.Helper()
		position, err := observeKaminoFromFixedAccounts(context.Background(),
			func(_ context.Context, _ []string, slot int64) (int64, []ConfirmedAccount, error) {
				return slot, []ConfirmedAccount{{Address: kaminoScopePrices, Lamports: 1, Data: []byte{1}}}, nil
			}, 77, accounts, config)
		if err != nil {
			t.Fatal(err)
		}
		return position
	}
	batch := routeNAVFixture(t, 77)
	for _, address := range []string{kaminoCollateralReserve, kaminoDebtReserve} {
		putKey(t, accountAt(batch, address).Data[5112:5144], kaminoScopePrices)
	}
	nav, err := ComputeRouteNAV(77, batch, manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !nav.ObligationPresent || nav.PositionCollateralValue != 30 || nav.PositionDebtValue != 7 {
		t.Fatalf("present obligation NAV: %+v", nav)
	}
	position := observe(append(batch, clockFixture()))
	if !position.ObligationPresent || !position.HasPosition || position.CollateralDepositedRaw != 10 {
		t.Fatalf("present obligation position: %+v", position)
	}

	absentBatch := flattenObligation(batch)
	absentNAV, err := ComputeRouteNAV(77, absentBatch, manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if absentNAV.ObligationPresent || absentNAV.PositionCollateralValue != 0 || absentNAV.PositionDebtValue != 0 {
		t.Fatalf("absent obligation NAV: %+v", absentNAV)
	}
	if absentNAV.StrategyNAVRaw != nav.StrategyNAVRaw-nav.PositionCollateralValue+nav.PositionDebtValue {
		t.Fatalf("absent obligation NAV kept position value: %+v", absentNAV)
	}
	absentPosition := observe(append(absentBatch, clockFixture()))
	if absentPosition.ObligationPresent || absentPosition.HasPosition || absentPosition.CollateralDepositedRaw != 0 || absentPosition.DebtRaw != 0 {
		t.Fatalf("absent obligation decoded into a flat position: %+v", absentPosition)
	}
}
