package backyardrwa

import (
	"encoding/binary"
	"testing"
)

// TestObservedExternalNAVExcludesCustodyBalance is the P2.1 acceptance proof.
// Voltr books the strategy custody balance itself (receipt offset 128) and
// credits it into totalValue when it is swept, so the worker's NAV counts only
// external value: Squads cash, idle collateral, and the Kamino net position.
// The Sep 4 incident reported the custody balance twice.
func TestObservedExternalNAVExcludesCustodyBalance(t *testing.T) {
	manifest := readyWorkerManifest(t)
	observe := func(strategyCustodyRaw uint64) RouteNAVSnapshot {
		t.Helper()
		accounts := routeNAVFixture(t, 77)
		binary.LittleEndian.PutUint64(accountAt(accounts, bridgeStrategyATA).Data[64:72], strategyCustodyRaw)
		nav, err := ComputeRouteNAV(77, accounts, manifest, nil)
		if err != nil {
			t.Fatal(err)
		}
		return nav
	}
	empty := observe(0)
	if empty.StrategyNAVRaw != 33 || empty.TotalVaultNAVRaw != 44 {
		t.Fatalf("external NAV fixture drifted: %+v", empty)
	}
	for _, custody := range []uint64{1, 5, 1_000_000, 3_793_417} {
		observed := observe(custody)
		if observed.StrategyNAVRaw != empty.StrategyNAVRaw || observed.TotalVaultNAVRaw != empty.TotalVaultNAVRaw {
			t.Fatalf("custody %d leaked into NAV: nav=%d total=%d", custody, observed.StrategyNAVRaw, observed.TotalVaultNAVRaw)
		}
		if observed.TotalVaultNAVRaw != observed.VaultIdleRaw+observed.StrategyNAVRaw {
			t.Fatalf("vault NAV identity broken while custody was counted: %+v", observed)
		}
		if observed.Custodies.StrategyUSDCraw != custody {
			t.Fatalf("custody balance was not kept as an observed field: %+v", observed)
		}
		if observed.Receipt.CustodyTrackedRaw != 0 {
			t.Fatalf("receipt custody tracking was not reported separately: %+v", observed)
		}
	}
}

// TestBridgePoststateNAVComposesExternalValueOnly proves the armed NAV for
// every lifecycle leg: allocation adds the staged cash to Squads, restoration
// adds nothing (the cash already left Squads when it was staged), and a
// refresh adds nothing at all.
func TestBridgePoststateNAVComposesExternalValueOnly(t *testing.T) {
	manifest := readyWorkerManifest(t)
	for _, tc := range []struct {
		name               string
		custody, squads    uint64
		wantNAV, wantTotal uint64
	}{
		{"allocate stages cash out of Squads into custody", 10, 16, 43, 54},
		{"restore sweeps custody without creating value", 0, 6, 33, 44},
		{"refresh reports the unchanged external book", 0, 6, 33, 44},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts := routeNAVFixture(t, 77)
			post := RouteNAVCustodies{
				VoltrIdleRaw: 11, StrategyUSDCraw: tc.custody, SquadsUSDCraw: tc.squads, SquadsPRIMEraw: 3,
			}
			nav, err := ComputeRouteNAV(77, accounts, manifest, &post)
			if err != nil {
				t.Fatal(err)
			}
			if nav.StrategyNAVRaw != tc.wantNAV || nav.TotalVaultNAVRaw != tc.wantTotal {
				t.Fatalf("custody=%d squads=%d: nav=%d want=%d total=%d want=%d",
					tc.custody, tc.squads, nav.StrategyNAVRaw, tc.wantNAV, nav.TotalVaultNAVRaw, tc.wantTotal)
			}
		})
	}
}

// TestRestoreAmountEqualsJournaledStagedAmount is the P2.2 acceptance proof:
// the restore amount must equal both the observed custody balance and the
// amount the journal recorded for the last stage, otherwise the custody is
// unexplained and the worker stops for manual recovery.
func TestRestoreAmountEqualsJournaledStagedAmount(t *testing.T) {
	for _, tc := range []struct {
		name       string
		custody    int64
		staged     int64
		known      bool
		wantAction Action
		wantAmount int64
		wantReason string
	}{
		{"matched journal restores exactly the staged amount", 5, 5, true, VoltrRestoreIdle, 5, "withdrawal_staged"},
		{"unknown journal never infers an amount", 5, 0, false, HoldManualRecovery, 0, "custody_mismatch"},
		{"divergent journaled amount never sweeps custody", 5, 4, true, HoldManualRecovery, 0, "custody_mismatch"},
		{"empty custody needs no journaled stage", 0, 0, false, StageSquadsToVoltr, 5, "withdrawal_demand"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := base()
			s.WithdrawalDemandRaw = 8
			s.VoltrIdleRaw = 3
			s.VoltrStrategyIdleRaw = tc.custody
			s.SquadsIdleRaw = 5
			s.StagedAmountKnown = tc.known
			s.StagedAmountRaw = tc.staged
			got := Decide(s)
			if got.Action != tc.wantAction || got.AmountRaw != tc.wantAmount || got.Reason != tc.wantReason {
				t.Fatalf("custody=%d journaled=%d known=%t: %+v", tc.custody, tc.staged, tc.known, got)
			}
		})
	}
}

// TestRefreshRequiresEmptyStrategyCustody covers the custody_residue HOLD: a
// report moves no capital, so it must observe the strategy custody empty.
func TestRefreshRequiresEmptyStrategyCustody(t *testing.T) {
	s := base()
	s.CapitalMutated = true
	s.LastReportAgeSeconds = 60
	s.VoltrStrategyIdleRaw = 5
	s.StagedAmountKnown, s.StagedAmountRaw = true, 5
	if got := Decide(s); got.Action != HoldManualRecovery || got.Reason != "custody_residue" || got.AmountRaw != 0 {
		t.Fatalf("refresh ignored a strategy custody residue: %+v", got)
	}
	resolved := s
	resolved.VoltrStrategyIdleRaw = 0
	if got := Decide(resolved); got.Action != ReportNAV || got.Reason != "nav_due" {
		t.Fatalf("empty custody was blocked from reporting: %+v", got)
	}
	// The non-USDC lane carries the same residue rule.
	canary := s
	canary.RouteLane = "AUTO/AUTO/PYUSD"
	if got := Decide(canary); got.Action != HoldManualRecovery || got.Reason != "custody_residue" {
		t.Fatalf("non-USDC refresh ignored a custody residue: %+v", got)
	}
}
