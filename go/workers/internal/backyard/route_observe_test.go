package backyard

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

func TestPilotUSDCObservationBoundsDepositRemainderWithoutLosingNAV(t *testing.T) {
	m := readyWorkerManifest(t)
	m.RuntimeActivation.SelectedLane = PrimeUSDCLaneID
	var previous string
	for _, valid := range []bool{true, false} {
		observe := productionConfirmedBatchForManifest(t, m, 77, func(accounts []ConfirmedAccount) {
			// Keep the real production decoders; replace only controlled chain
			// bytes. A collateral-only obligation avoids a separate debt window.
			clear(accountAt(accounts, primePRIMEUSDC.Kamino.Obligation).Data[1208:2208])
			binary.LittleEndian.PutUint64(accountAt(accounts, budgetClockAddress).Data[:8], 77)
			reserve := accountAt(accounts, primePRIMEUSDC.Kamino.CollateralReserve).Data
			for i := 1; i < 11; i++ {
				binary.LittleEndian.PutUint32(reserve[kaminoReserveConfigOffset+64+i*8:], 10_000)
			}
			if !valid {
				reserve[kaminoReserveConfigOffset+7] = 1
			}
		})
		o, err := observe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		s := o.Snapshot
		if s.RouteLane != PrimeUSDCLaneID || s.CollateralIdleRaw != 3 || s.StrategyNAVRaw != 40 || s.PositionCollateralRaw != 10 {
			t.Fatal("deposit bound changed custody or NAV", s)
		}
		if valid && s.MinimumCollateralDepositRaw != 3 || !valid && s.MinimumCollateralDepositRaw != 0 {
			t.Fatal("production observation omitted reserve rounding bound", s.MinimumCollateralDepositRaw)
		}
		if !valid && s.ObservationID == previous {
			t.Fatal("changed rounding evidence reused observation identity")
		}
		previous = s.ObservationID
	}
}

func TestForeignIdleAcknowledgmentComesFromObservedVault(t *testing.T) {
	t.Parallel()
	for _, amount := range []int64{currentRWAForeignIdleRaw, looping.AcknowledgedForeignIdleRaw} {
		m := readyWorkerManifest(t)
		v := *m.vault
		v.AcknowledgedForeignIdleRaw = amount
		m.vault = &v
		o, err := productionConfirmedBatchForManifest(t, m, 77, nil)(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if o.Snapshot.AcknowledgedForeignIdleRaw != amount {
			t.Fatalf("observer lost vault-specific acknowledgment: got %d want %d", o.Snapshot.AcknowledgedForeignIdleRaw, amount)
		}
	}
}

func cadenceNAV(slot, current, reported uint64, lastUpdated time.Time) RouteNAVSnapshot {
	digest := navDigest(strings.Repeat("a", 64))
	return RouteNAVSnapshot{
		Slot: int64(slot), StrategyNAVRaw: current, TotalVaultNAVRaw: current, PriorReportedNAVRaw: reported,
		PriorReportUpdatedTS: uint64(lastUpdated.Unix()), SnapshotDigest: digest,
		Voltr:  VoltrVaultBook{Vault: voltr.Vault{AdminPerformanceFeeBPS: uint16(rwa.AdminPerformanceFeeBPS)}},
		Report: BridgeReport{Sequence: slot, ObservedSlot: slot, NAVAfterRaw: current, SnapshotDigest: digest},
	}
}

// cadenceArmedIdentity completes the monitor inputs the production observe path
// adds for a route whose pinned program identity verified.
func cadenceArmedIdentity(s *Snapshot) {
	s.ProgramIdentityKnown = true
	s.VoltrProgramDeploySlot = voltrProgramDeploySlot
	s.AdaptorProgramDeploySlot = adaptorProgramDeploySlot
}

func TestOptionalLifecycleObligationPrefersSelectedPhase2Close(t *testing.T) {
	t.Parallel()
	selected := mapleSyrupUSDCUSDC.Kamino.Obligation
	if got := optionalLifecycleObligations(rwa, []string{primePRIMEUSDC.Kamino.Obligation, selected}); len(got) != 2 || got[0] != selected || got[1] != primePRIMEUSDC.Kamino.Obligation {
		t.Fatalf("both closed lifecycle obligations must be optional: %v", got)
	}
	if got := optionalLifecycleObligations(rwa, []string{primePRIMEUSDC.Kamino.Obligation}); len(got) != 1 || got[0] != primePRIMEUSDC.Kamino.Obligation {
		t.Fatalf("Phase 1 obligation fallback drifted: %v", got)
	}
	if got := optionalLifecycleObligations(rwa, []string{bridgeSquadsATA}); len(got) != 0 {
		t.Fatalf("unrelated account became optional: %v", got)
	}
	for _, obligation := range []string{primePRIMEPYUSD.Kamino.Obligation, primePRIMEUSDS.Kamino.Obligation} {
		if got := optionalLifecycleObligations(rwa, []string{bridgeSquadsATA, obligation}); len(got) != 1 || got[0] != obligation {
			t.Fatalf("closed Prime sibling obligation blocks terminal observation: %v", got)
		}
	}
}

func TestRouteNAVCadenceDoesNotSpamUnchangedFreshReports(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	snapshot := base()
	nav := cadenceNAV(uint64(snapshot.Slot), 42, 42, now.Add(-10*time.Second))
	if err := applyRouteNAVSnapshot(&snapshot, nav, now); err != nil {
		t.Fatal(err)
	}
	snapshot.VoltrTotalValueRaw = 42 // idle 0 + custody 0 + receipt 42
	cadenceArmedIdentity(&snapshot)
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

func testEconomicID(bridge string, prime, collateral, debt uint64, blocked bool, strategyNAV, reportedNAV, capacity uint64) string {
	return routeEconomicState{primeRaw: prime, collateralRaw: collateral, debtRaw: debt, borrowUtilizationBlocked: blocked,
		strategyNAVRaw: strategyNAV, priorReportedNAVRaw: reportedNAV, pairCapacityRaw: capacity}.observationID(bridge)
}

func TestRouteEconomicObservationIdentityExcludesReportSlot(t *testing.T) {
	t.Parallel()
	first := testEconomicID("bridge-state", 10, 20, 7, false, 42, 41, 100)
	// The report slot/sequence is intentionally not an input to this helper. Two
	// coherent observations with the same economics therefore remain identical.
	second := testEconomicID("bridge-state", 10, 20, 7, false, 42, 41, 100)
	if first != second {
		t.Fatalf("unchanged economic observation changed identity: %s != %s", first, second)
	}
	changed := testEconomicID("bridge-state", 10, 21, 7, false, 42, 41, 100)
	if changed == first {
		t.Fatal("changed collateral aliased to the prior economic observation")
	}
	utilizationBlocked := testEconomicID("bridge-state", 10, 20, 7, true, 42, 41, 100)
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
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	snapshot := base()
	snapshot.SquadsIdleRaw = 100
	nav := cadenceNAV(uint64(snapshot.Slot), 2_000, 42, now.Add(-10*time.Second))
	if err := applyRouteNAVSnapshot(&snapshot, nav, now); err != nil {
		t.Fatal(err)
	}
	snapshot.VoltrTotalValueRaw = 42 // idle 0 + custody 0 + receipt 42
	snapshot.StrategyNAVRaw = 2_000  // the recomputed external NAV this merge observed
	cadenceArmedIdentity(&snapshot)
	// The merge no longer reads a capital mutation out of the NAV comparison:
	// that fact comes from the reconciled journal (productionObserveState.observe).
	if snapshot.CapitalMutated {
		t.Fatal("the NAV comparison alone marked a capital mutation")
	}
	snapshot.CapitalMutated = true // reconciled bridge mutation newer than the last report
	if got := Decide(snapshot); got.Action != ReportNAV || got.Reason != "nav_due" {
		t.Fatalf("risk mutation was selected before NAV report: %+v", got)
	}
}

func TestRouteNAVCadenceReportsReconciledRiskMutationEvenWhenValueIsUnchanged(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	snapshot := base()
	snapshot.SquadsIdleRaw = 100
	nav := cadenceNAV(uint64(snapshot.Slot), 42, 42, now.Add(-10*time.Second))
	if err := applyRouteNAVSnapshot(&snapshot, nav, now); err != nil {
		t.Fatal(err)
	}
	snapshot.PostMutationNAVRequired = true
	snapshot.VoltrTotalValueRaw = 42 // idle 0 + custody 0 + receipt 42
	cadenceArmedIdentity(&snapshot)
	if snapshot.CapitalMutated {
		t.Fatal("unchanged value was incorrectly marked as capital mutation")
	}
	if got := Decide(snapshot); got.Action != ReportNAV {
		t.Fatalf("reconciled risk mutation advanced without its accounting report: %+v", got)
	}
}

func TestRouteNAVCadenceReportsAtOneHour(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	snapshot := base()
	nav := cadenceNAV(uint64(snapshot.Slot), 42, 42, now.Add(-time.Hour))
	if err := applyRouteNAVSnapshot(&snapshot, nav, now); err != nil {
		t.Fatal(err)
	}
	snapshot.VoltrTotalValueRaw = 42 // idle 0 + custody 0 + receipt 42
	cadenceArmedIdentity(&snapshot)
	if got := Decide(snapshot); got.Action != ReportNAV || snapshot.LastReportAgeSeconds != 3600 {
		t.Fatalf("aged NAV did not report: snapshot=%+v decision=%+v", snapshot, got)
	}
}

func TestRouteNAVCadenceRejectsMixedSlotsAndBoundsFutureClockSkew(t *testing.T) {
	t.Parallel()
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

// The view holds every account the route observer reads, on every lane.
func TestViewHoldsEveryRouteObservationAccount(t *testing.T) {
	t.Parallel()
	held := map[string]bool{}
	for _, address := range viewAddresses() {
		held[address] = true
	}
	for _, address := range routeFixedAddresses(rwa) {
		if !held[address] {
			t.Fatalf("the observation reads %s outside the view", address)
		}
	}
}

// Review of #396: an entry that started while the queue was covered swapped
// its cash to collateral, then held stale with that collateral undeposited
// until the queue was claimed.
func TestCoveredDemandObservationDepositsSwappedCollateral(t *testing.T) {
	m := primeFixtureManifest(readyWorkerManifest(t))
	m.RuntimeActivation.SelectedLane = PrimeUSDCLaneID
	accounts := productionRouteBatchAccounts(t, 77, func(accounts []ConfirmedAccount) {
		clear(accountAt(accounts, primePRIMEUSDC.Kamino.Obligation).Data[1208:2208])
		binary.LittleEndian.PutUint64(accountAt(accounts, budgetClockAddress).Data[:8], 77)
		reserve := accountAt(accounts, primePRIMEUSDC.Kamino.CollateralReserve).Data
		for i := 1; i < 11; i++ {
			binary.LittleEndian.PutUint32(reserve[kaminoReserveConfigOffset+64+i*8:], 10_000)
		}
		binary.LittleEndian.PutUint64(accountAt(accounts, primePRIMEUSDC.CollateralCustody).Data[64:72], 4_000)
	})
	address, data := receiptFixture(t, m.vault.VoltrVault, testPublicKey(44), 5, 5<<48)
	read := fixtureBatchRuntime(77, accounts)
	o, _, err := observeConfirmedRouteSnapshotWithAccounts(context.Background(), m, routeObservationRuntime{state: testVaultState, inputs: targetInputs{ExitCosts: fixtureExitCosts()},
		read: func(ctx context.Context, addresses []string, minSlot int64) (int64, []ConfirmedAccount, []programAccount, error) {
			slot, observed, _, err := read(ctx, addresses, minSlot)
			return slot, observed, []programAccount{{Address: address, Account: ConfirmedAccount{Address: address, Owner: voltr.ProgramID.String(), Lamports: 1, Data: data}}}, err
		},
		refreshValuation: fixtureRefresh(77, accounts),
		now:              func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	})
	if err != nil {
		t.Fatal(err)
	}
	s := base()
	s.RouteLane, s.StrategyKey = PrimeUSDCLaneID, PrimeUSDCLaneID
	s.WithdrawalDemandRaw, s.VoltrIdleRaw = o.Snapshot.WithdrawalDemandRaw, o.Snapshot.VoltrIdleRaw
	s.CollateralIdleRaw, s.MinimumCollateralDepositRaw = o.Snapshot.CollateralIdleRaw, o.Snapshot.MinimumCollateralDepositRaw
	if got := Decide(targeted(s)); s.WithdrawalDemandRaw != 5 || s.VoltrIdleRaw < 5 || got.Action != OpenRouteStep || got.AmountRaw != 4_000 {
		t.Fatalf("covered demand %d of idle %d left collateral undeposited: %+v", s.WithdrawalDemandRaw, s.VoltrIdleRaw, got)
	}
}
