package backyardrwa

import (
	"strings"
	"testing"
	"time"
)

func cadenceNAV(slot, current, reported uint64, lastUpdated time.Time) RouteNAVSnapshot {
	digest := strings.Repeat("a", 64)
	return RouteNAVSnapshot{
		Slot: int64(slot), StrategyNAVRaw: current, TotalVaultNAVRaw: current, PriorReportedNAVRaw: reported,
		PriorReportUpdatedTS: uint64(lastUpdated.Unix()), SnapshotDigest: digest,
		Report: BridgeReport{Sequence: slot, ObservedSlot: slot, NAVAfterRaw: current, SnapshotDigest: digest},
	}
}

func TestOptionalLifecycleObligationPrefersSelectedPhase2Close(t *testing.T) {
	selected := mapleSyrupUSDCUSDC.Kamino.Obligation
	if got := optionalLifecycleObligations([]string{kaminoPrimeUSDCObligation, selected}); len(got) != 2 || got[0] != selected || got[1] != kaminoPrimeUSDCObligation {
		t.Fatalf("both closed lifecycle obligations must be optional: %v", got)
	}
	if got := optionalLifecycleObligations([]string{kaminoPrimeUSDCObligation}); len(got) != 1 || got[0] != kaminoPrimeUSDCObligation {
		t.Fatalf("Phase 1 obligation fallback drifted: %v", got)
	}
	if got := optionalLifecycleObligations([]string{bridgeSquadsATA}); len(got) != 0 {
		t.Fatalf("unrelated account became optional: %v", got)
	}
}

func TestRouteNAVCadenceDoesNotSpamUnchangedFreshReports(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	snapshot := base()
	nav := cadenceNAV(uint64(snapshot.Slot), 42, 42, now.Add(-10*time.Second))
	if err := applyRouteNAVSnapshot(&snapshot, nav, now); err != nil {
		t.Fatal(err)
	}
	if snapshot.CapitalMutated || snapshot.LastReportAgeSeconds != 10 {
		t.Fatalf("unchanged NAV was marked dirty: %+v", snapshot)
	}
	if snapshot.TotalVaultNAVRaw != 42 || snapshot.PriorReportedNAVRaw != 42 ||
		snapshot.PriorReportUpdatedUnix != now.Add(-10*time.Second).Unix() ||
		snapshot.ReportSequence != snapshot.Slot || snapshot.ReportSnapshotDigest != strings.Repeat("a", 64) {
		t.Fatalf("persistable NAV/report projection was discarded: %+v", snapshot)
	}
	if got := Decide(snapshot); got.Action == ReportNAV {
		t.Fatalf("unchanged fresh NAV spammed a report: %+v", got)
	}
}

func TestRouteEconomicObservationIdentityExcludesReportSlot(t *testing.T) {
	first := routeEconomicObservationID("bridge-state", 10, 20, 7, true, true, false, 42, 41, 100)
	// The report slot/sequence is intentionally not an input to this helper. Two
	// coherent observations with the same economics therefore remain identical.
	second := routeEconomicObservationID("bridge-state", 10, 20, 7, true, true, false, 42, 41, 100)
	if first != second {
		t.Fatalf("unchanged economic observation changed identity: %s != %s", first, second)
	}
	changed := routeEconomicObservationID("bridge-state", 10, 21, 7, true, true, false, 42, 41, 100)
	if changed == first {
		t.Fatal("changed collateral aliased to the prior economic observation")
	}
	utilizationBlocked := routeEconomicObservationID("bridge-state", 10, 20, 7, true, true, true, 42, 41, 100)
	if utilizationBlocked == first {
		t.Fatal("changed debt-reserve utilization admission aliased to the prior economic observation")
	}
	firstSnapshot, laterSnapshot := base(), base()
	firstSnapshot.ObservationID, laterSnapshot.ObservationID = first, second
	laterSnapshot.Slot++
	if Decide(firstSnapshot).IdempotencyKey != Decide(laterSnapshot).IdempotencyKey {
		t.Fatal("a later slot changed the durable decision for unchanged economics")
	}
}

func TestRouteNAVCadenceReportsMutationBeforeNextRiskAction(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	snapshot := base()
	snapshot.SquadsIdleRaw = 100
	nav := cadenceNAV(uint64(snapshot.Slot), 43, 42, now.Add(-10*time.Second))
	if err := applyRouteNAVSnapshot(&snapshot, nav, now); err != nil {
		t.Fatal(err)
	}
	if !snapshot.CapitalMutated {
		t.Fatal("changed independently computed NAV was not marked dirty")
	}
	if got := Decide(snapshot); got.Action != ReportNAV || got.Reason != "nav_due" {
		t.Fatalf("risk mutation was selected before NAV report: %+v", got)
	}
}

func TestRouteNAVCadenceReportsReconciledRiskMutationEvenWhenValueIsUnchanged(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	snapshot := base()
	snapshot.SquadsIdleRaw = 100
	nav := cadenceNAV(uint64(snapshot.Slot), 42, 42, now.Add(-10*time.Second))
	if err := applyRouteNAVSnapshot(&snapshot, nav, now); err != nil {
		t.Fatal(err)
	}
	snapshot.PostMutationNAVRequired = true
	if snapshot.CapitalMutated {
		t.Fatal("unchanged value was incorrectly marked as capital mutation")
	}
	if got := Decide(snapshot); got.Action != ReportNAV {
		t.Fatalf("reconciled risk mutation advanced without its accounting report: %+v", got)
	}
}

func TestRouteNAVCadenceReportsAtSixtySeconds(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	snapshot := base()
	nav := cadenceNAV(uint64(snapshot.Slot), 42, 42, now.Add(-60*time.Second))
	if err := applyRouteNAVSnapshot(&snapshot, nav, now); err != nil {
		t.Fatal(err)
	}
	if got := Decide(snapshot); got.Action != ReportNAV || snapshot.LastReportAgeSeconds != 60 {
		t.Fatalf("aged NAV did not report: snapshot=%+v decision=%+v", snapshot, got)
	}
}

func TestRouteNAVCadenceRejectsMixedSlotsAndBoundsFutureClockSkew(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	snapshot := base()
	mixed := cadenceNAV(uint64(snapshot.Slot+1), 42, 42, now)
	if err := applyRouteNAVSnapshot(&snapshot, mixed, now); err == nil {
		t.Fatal("mixed-slot NAV merged into route snapshot")
	}

	snapshot = base()
	future := cadenceNAV(uint64(snapshot.Slot), 42, 42, now.Add(time.Second))
	if err := applyRouteNAVSnapshot(&snapshot, future, now); err != nil {
		t.Fatal(err)
	}
	if snapshot.LastReportAgeSeconds != 0 {
		t.Fatalf("future receipt timestamp produced a negative/unbounded age: %d", snapshot.LastReportAgeSeconds)
	}
}

func TestRouteFixedAddressesIncludeEveryMutableConstructionInput(t *testing.T) {
	addresses := routeFixedAddresses(readyWorkerManifest(t))
	wanted := map[string]bool{bridgeIdleATA: false, bridgeStrategyATA: false, bridgeSquadsATA: false, kaminoPrimeCustody: false, kaminoPrimeUSDCObligation: false, kaminoCollateralReserve: false, kaminoDebtReserve: false, kaminoPrimeLiquiditySupply: false, kaminoUSDCLiquiditySupply: false, reportTicketPDA: false}
	for _, address := range addresses {
		if _, ok := wanted[address]; ok {
			wanted[address] = true
		}
	}
	for address, present := range wanted {
		if !present {
			t.Fatalf("fixed account batch omitted %s", address)
		}
	}
}

func TestReceiptFenceRequiresOrderedIdenticalDemand(t *testing.T) {
	if !stableReceiptFence(10, 11, 12, 7, 7, "fingerprint", "fingerprint") {
		t.Fatal("ordered identical receipt fence was rejected")
	}
	for _, test := range []struct {
		before, fixed, after, left, right int64
		leftFP, rightFP                   string
	}{
		{11, 10, 12, 7, 7, "f", "f"},
		{10, 12, 11, 7, 7, "f", "f"},
		{10, 11, 12, 7, 8, "f", "f"},
		{10, 11, 12, 7, 7, "f", "changed"},
	} {
		if stableReceiptFence(test.before, test.fixed, test.after, test.left, test.right, test.leftFP, test.rightFP) {
			t.Fatalf("unstable receipt fence accepted: %+v", test)
		}
	}
}
