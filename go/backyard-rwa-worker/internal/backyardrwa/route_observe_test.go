package backyardrwa

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"
)

func cadenceNAV(slot, current, reported uint64, lastUpdated time.Time) RouteNAVSnapshot {
	digest := strings.Repeat("a", 64)
	return RouteNAVSnapshot{
		Slot: int64(slot), StrategyNAVRaw: current, PriorReportedNAVRaw: reported,
		PriorReportUpdatedTS: uint64(lastUpdated.Unix()), SnapshotDigest: digest,
		Report: BridgeReport{Sequence: slot, ObservedSlot: slot, NAVAfterRaw: current, SnapshotDigest: digest},
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
	if got := Decide(snapshot); got.Action == ReportNAV {
		t.Fatalf("unchanged fresh NAV spammed a report: %+v", got)
	}
}

func TestRouteEconomicObservationIdentityExcludesReportSlot(t *testing.T) {
	first := routeEconomicObservationID("bridge-state", 10, 20, 7, true, true, 42, 41, 100)
	// The report slot/sequence is intentionally not an input to this helper. Two
	// coherent observations with the same economics therefore remain identical.
	second := routeEconomicObservationID("bridge-state", 10, 20, 7, true, true, 42, 41, 100)
	if first != second {
		t.Fatalf("unchanged economic observation changed identity: %s != %s", first, second)
	}
	changed := routeEconomicObservationID("bridge-state", 10, 21, 7, true, true, 42, 41, 100)
	if changed == first {
		t.Fatal("changed collateral aliased to the prior economic observation")
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

func TestRouteObservationRetriesMixedNAVSlotBeforeMerging(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	manifest := readyWorkerManifest(t)
	accountCalls := 0
	accounts := routeNAVFixture(t, 10)
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	oneAndHalf := new(big.Int).Mul(big.NewInt(3), new(big.Int).Lsh(big.NewInt(1), 59))
	var primePrice, debtPrice [16]byte
	putScaledFraction(primePrice[:], oneAndHalf)
	putScaledFraction(debtPrice[:], one)
	runtime := routeObservationRuntime{
		bridge: func(context.Context) (Observation, error) {
			snapshot := base()
			snapshot.Slot = 10
			snapshot.VoltrIdleRaw, snapshot.VoltrStrategyIdleRaw, snapshot.SquadsIdleRaw = 11, 5, 6
			return Observation{Snapshot: snapshot, ObservedAt: now}, nil
		},
		kamino: func(context.Context) (KaminoPosition, error) {
			return KaminoPosition{
				Slot: 10, RefreshedSlot: 10, HasPosition: true, CollateralDepositedRaw: 10,
				DebtRaw: 7, RedeemablePrimeRaw: 20, CollateralPriceSF: primePrice,
				DebtPriceSF: debtPrice, LiquidationThresholdBPS: 8000,
			}, nil
		},
		accounts: func(_ context.Context, _ []string, minimumSlot int64) (int64, []ConfirmedAccount, error) {
			if minimumSlot != 10 {
				t.Fatalf("policy read used wrong minimum slot: %d", minimumSlot)
			}
			accountCalls++
			slot := int64(10)
			if accountCalls == 1 {
				slot = 11
			}
			return slot, accounts, nil
		},
		now: func() time.Time { return now },
	}
	observation, err := observeConfirmedRouteSnapshot(context.Background(), manifest, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if accountCalls != 2 || observation.Snapshot.Slot != 10 || observation.Snapshot.LastReportAgeSeconds != 0 {
		t.Fatalf("mixed account slot was not retried coherently: calls=%d observation=%+v", accountCalls, observation)
	}
}
