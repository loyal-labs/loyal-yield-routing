package backyardrwa

import (
	"encoding/binary"
	"testing"
	"time"
)

// monitorSnapshot builds a snapshot exactly the way the serialized worker does:
// one coherent fixed account batch decoded into a route NAV, merged onto a
// Snapshot (which arms the monitors), then completed with the journal, ticket,
// and program-identity inputs the production observe path adds. The baseline
// book is coherent: idle 11 + custody 0 + receipt 42 = 53 totalValue, and the
// external NAV 33 sits inside the receipt tolerance floor.
func monitorSnapshot(t *testing.T, mutate func(accounts []ConfirmedAccount, s *Snapshot)) Snapshot {
	t.Helper()
	accounts := routeNAVFixture(t, 77)
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeStrategyATA).Data[64:72], 0)
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeVoltrVault).Data[168:176], 53)
	nav, err := ComputeRouteNAV(77, accounts, readyWorkerManifest(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	s := Snapshot{ObservationID: "monitor", Slot: 77, RouteKind: RouteKind, Fresh: true,
		RouteLane: RouteID, StrategyKey: RouteID,
		VoltrIdleRaw: int64(nav.Custodies.VoltrIdleRaw), VoltrStrategyIdleRaw: int64(nav.Custodies.StrategyUSDCraw),
		SquadsIdleRaw: int64(nav.Custodies.SquadsUSDCraw), PrimeIdleRaw: int64(nav.Custodies.SquadsPRIMEraw),
		CollateralIdleRaw: int64(nav.Custodies.SquadsPRIMEraw), StrategyNAVRaw: int64(nav.StrategyNAVRaw)}
	if err := applyRouteNAVSnapshot(&s, nav, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !s.MonitorsArmed {
		t.Fatal("coherent confirmed batch did not arm the monitors")
	}
	s.TicketLastConsumedSequenceRaw = 4
	s.JournalSequenceKnown, s.JournalReconciledSequenceRaw = true, 4
	s.JournalArmedNAVKnown, s.JournalArmedNAVRaw = true, int64(nav.PriorReportedNAVRaw)
	s.ProgramIdentityKnown = true
	s.VoltrProgramDeploySlot = voltrProgramDeploySlot
	s.AdaptorProgramDeploySlot = adaptorProgramDeploySlot
	if mutate != nil {
		mutate(accounts, &s)
	}
	return s
}

func TestMonitorBookIdentityHoldsOnVaultMismatch(t *testing.T) {
	if hold, blocked := bridgeMonitorHold(monitorSnapshot(t, nil)); blocked {
		t.Fatalf("coherent book held: %+v", hold)
	}
	s := monitorSnapshot(t, func(_ []ConfirmedAccount, s *Snapshot) { s.VoltrTotalValueRaw = 49 })
	got := Decide(s)
	if got.Action != HoldManualRecovery || got.Reason != "book_identity_mismatch" {
		t.Fatalf("vault book mismatch did not hold: %+v", got)
	}
}

func TestMonitorReceiptMatchesArmedNAVAndObservedExternal(t *testing.T) {
	healthy := monitorSnapshot(t, nil)
	if hold, blocked := bridgeMonitorHold(healthy); blocked {
		t.Fatalf("matched receipt held: %+v", hold)
	}
	stale := monitorSnapshot(t, func(_ []ConfirmedAccount, s *Snapshot) { s.JournalArmedNAVRaw = 43 })
	if got := Decide(stale); got.Action != HoldManualRecovery || got.Reason != "receipt_nav_mismatch" {
		t.Fatalf("receipt did not have to match the armed NAV: %+v", got)
	}
	drifted := monitorSnapshot(t, func(accounts []ConfirmedAccount, s *Snapshot) {
		binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], 20_000)
		s.SquadsIdleRaw = 20_000
		s.StrategyNAVRaw += 19_994
		s.CapitalMutated = false
	})
	if got := Decide(drifted); got.Action != HoldManualRecovery || got.Reason != "nav_drift_unexplained" {
		t.Fatalf("external NAV drift was not bounded: %+v", got)
	}
	mutating := drifted
	mutating.PostMutationNAVRequired = true
	if hold, blocked := bridgeMonitorHold(mutating); blocked {
		t.Fatalf("an unreported reconciled mutation must defer the receipt check: %+v", hold)
	}
}

func TestMonitorCustodyZeroAtRestAndStageAmountPresent(t *testing.T) {
	resting := monitorSnapshot(t, nil)
	if resting.VoltrStrategyIdleRaw != 0 || resting.VoltrReceiptCustodyTrackedRaw != 0 {
		t.Fatalf("resting fixture is not flat: %+v", resting)
	}
	// The real stage transient: the stage moved Squads cash into the custody ATA
	// without invoking Voltr, so tv, the receipt position, and the custody Voltr
	// books are all unchanged while the ATA holds exactly the staged amount.
	staged := monitorSnapshot(t, func(accounts []ConfirmedAccount, s *Snapshot) {
		binary.LittleEndian.PutUint64(accountAt(accounts, bridgeStrategyATA).Data[64:72], 7)
		s.VoltrStrategyIdleRaw = 7
		s.StagedAmountKnown, s.StagedAmountRaw = true, 7
		s.StageTransient = true
		s.WithdrawalDemandRaw = 20 // uncovered by idle 11, so the stage leg restores
	})
	if got := Decide(staged); got.Action != VoltrRestoreIdle || got.AmountRaw != 7 {
		t.Fatalf("explained custody was not restored at its staged amount: %+v", got)
	}
	// Voltr still books zero during the transient, so a nonzero tracked custody
	// is a book fault rather than a stage in flight.
	booked := staged
	booked.VoltrReceiptCustodyTrackedRaw = 7
	booked.VoltrTotalValueRaw = 60 // idle 11 + receipt 42 + tracked 7
	if got := Decide(booked); got.Action != HoldManualRecovery || got.Reason != "custody_transient_mismatch" {
		t.Fatalf("a transient Voltr had already booked did not hold: %+v", got)
	}
	over := staged
	over.StagedAmountRaw = 6
	// Custody discipline runs before the monitors, so Decide names the journal
	// disagreement; the monitor itself names the transient it rejected.
	if got := Decide(over); got.Action != HoldManualRecovery || got.Reason != "custody_mismatch" {
		t.Fatalf("custody above the staged amount did not hold: %+v", got)
	}
	if hold, blocked := bridgeMonitorHold(over); !blocked || hold.Reason != "custody_transient_mismatch" {
		t.Fatalf("a transient above its staged amount passed the custody monitor: %+v blocked=%t", hold, blocked)
	}
	residue := staged
	residue.StageTransient = false
	if got := Decide(residue); got.Action != HoldManualRecovery || got.Reason != "custody_residue" {
		t.Fatalf("custody at rest did not hold: %+v", got)
	}
}

func TestMonitorIdleCoversPendingRequestQuotes(t *testing.T) {
	s := monitorSnapshot(t, func(_ []ConfirmedAccount, s *Snapshot) {
		s.PostMutationNAVRequired = true
		s.WithdrawalDemandRaw = 20
	})
	if got := Decide(s); got.Action == ReportNAV || got.Action == VoltrAllocateToSquads || got.Action == HoldManualRecovery {
		t.Fatalf("underfunded queue allowed a refresh or allocation tick: %+v", got)
	}
	if got := Decide(s); got.Action != SwapPrimeToUSDCStep {
		t.Fatalf("underfunded queue did not keep the unwind leg admissible: %+v", got)
	}
	disarmed := s
	disarmed.MonitorsArmed = false
	if got := Decide(disarmed); got.Action != ReportNAV {
		t.Fatalf("monitor gate did not control the refresh block: %+v", got)
	}
}

func TestMonitorProgramDataPinHoldsOnChange(t *testing.T) {
	if hold, blocked := bridgeMonitorHold(monitorSnapshot(t, nil)); blocked {
		t.Fatalf("pinned program identity held: %+v", hold)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{"voltr upgrade", func(s *Snapshot) { s.VoltrProgramDeploySlot = voltrProgramDeploySlot + 1 }},
		{"adaptor upgrade", func(s *Snapshot) { s.AdaptorProgramDeploySlot = adaptorProgramDeploySlot + 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := monitorSnapshot(t, nil)
			tc.mutate(&s)
			if got := Decide(s); got.Action != HoldManualRecovery || got.Reason != "program_identity_changed" {
				t.Fatalf("changed program identity did not hold: %+v", got)
			}
		})
	}
	unknown := monitorSnapshot(t, nil)
	unknown.ProgramIdentityKnown = false
	if got := Decide(unknown); got.Action != HoldManualRecovery || got.Reason != "program_identity_unverified" {
		t.Fatalf("an unverified program identity read did not hold durably: %+v", got)
	}
}

func TestMonitorFeeAccumulatorsBoundedAndFeesZero(t *testing.T) {
	accounts := routeNAVFixture(t, 77)
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeStrategyATA).Data[64:72], 0)
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeVoltrVault).Data[168:176], 53)
	nav, err := ComputeRouteNAV(77, accounts, readyWorkerManifest(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if nav.Voltr.FeeAccumulatorRaw() != 0 || nav.Voltr.LPSupplyInclFeesRaw(nav.LPSupplyRaw) != 2_000 {
		t.Fatalf("fee fixture drifted: %+v", nav.Voltr)
	}
	s := monitorSnapshot(t, func(_ []ConfirmedAccount, s *Snapshot) { s.FeeAccumulatorRaw = 100 })
	if got := Decide(s); got.Action != HoldManualRecovery || got.Reason != "fee_accumulator_anomaly" {
		t.Fatalf("unbounded fee accumulator did not hold: %+v", got)
	}
	within := monitorSnapshot(t, func(_ []ConfirmedAccount, s *Snapshot) { s.FeeAccumulatorRaw = 20 })
	if hold, blocked := bridgeMonitorHold(within); blocked {
		t.Fatalf("fee accumulator inside its bound held: %+v", hold)
	}
}

func TestMonitorSingleReporterSequenceMismatchHolds(t *testing.T) {
	s := monitorSnapshot(t, func(_ []ConfirmedAccount, s *Snapshot) { s.TicketLastConsumedSequenceRaw = 5 })
	if got := Decide(s); got.Action != HoldManualRecovery || got.Reason != "out_of_band_crank" {
		t.Fatalf("ticket sequence moved outside the journal: %+v", got)
	}
	unjournaled := monitorSnapshot(t, func(_ []ConfirmedAccount, s *Snapshot) {
		s.JournalSequenceKnown = false
		s.TicketLastConsumedSequenceRaw = 1
	})
	if got := Decide(unjournaled); got.Action != HoldManualRecovery || got.Reason != "out_of_band_crank" {
		t.Fatalf("consumed ticket without a journal operation did not hold: %+v", got)
	}
	fresh := monitorSnapshot(t, func(_ []ConfirmedAccount, s *Snapshot) {
		s.JournalSequenceKnown = false
		s.TicketLastConsumedSequenceRaw = 0
	})
	if hold, blocked := bridgeMonitorHold(fresh); blocked {
		t.Fatalf("unconsumed ticket without journal history held: %+v", hold)
	}
}
