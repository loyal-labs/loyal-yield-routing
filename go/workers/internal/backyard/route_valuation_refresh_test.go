package backyard

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestRouteRefreshValuesNoncashFromOneBankAndRejectsStaleOracle(t *testing.T) {
	t.Cleanup(func() { kaminoStaleHolds.Store(0) })
	for _, staleOracle := range []bool{false, true} {
		// Pin the third stale hold in a row, the one that latches.
		kaminoStaleHolds.Store(refreshSimulationLatchAfter - 1)
		m := readyWorkerManifest(t)
		m.RuntimeActivation.SelectedLane = PhaseOneLaneID
		initial := productionRouteBatchAccounts(t, 77, func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint64(accountAt(a, budgetClockAddress).Data[:8], 77)
			binary.LittleEndian.PutUint64(accountAt(a, kaminoCollateralReserve).Data[16:24], 1)
			binary.LittleEndian.PutUint64(accountAt(a, kaminoDebtReserve).Data[16:24], 1)
		})
		captured := productionRouteBatchAccounts(t, 78, func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint64(accountAt(a, budgetClockAddress).Data[:8], 78)
			for _, address := range []string{kaminoCollateralReserve, kaminoDebtReserve} {
				for i := 1; i < 11; i++ {
					binary.LittleEndian.PutUint32(accountAt(a, address).Data[kaminoReserveConfigOffset+64+i*8:], 10_000)
				}
			}
			// A chain custody mutation between the first read and capture must be
			// valued using the new bank, never old balances plus new reserve prices.
			binary.LittleEndian.PutUint64(accountAt(a, bridgeSquadsATA).Data[64:72], 21)
			if staleOracle {
				binary.LittleEndian.PutUint64(accountAt(a, kaminoCollateralReserve).Data[kaminoMarketPriceLastUpdatedTSOffset:], 1)
			}
		})
		refreshCalls := 0
		o, accounts, err := observeConfirmedRouteSnapshotWithAccounts(context.Background(), m, routeObservationRuntime{
			read: fixtureBatchRuntime(77, initial),
			now:  func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
			refreshValuation: func(_ context.Context, _ RuntimeRoute, addresses []string, min int64) (int64, []ConfirmedAccount, error) {
				refreshCalls++
				out := make([]ConfirmedAccount, len(addresses))
				for i, address := range addresses {
					out[i] = accountAt(captured, address)
					out[i].Address = address
					out[i].ValuationSource = routeRefreshValuationSource
					out[i].ValuationSlot = 78
				}
				return 78, out, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if refreshCalls != 1 {
			t.Fatal("refresh was not used for noncash exposure", refreshCalls)
		}
		if staleOracle {
			if o.Snapshot.ManualReason != "kamino_stale" || o.Snapshot.Fresh {
				t.Fatal("stale oracle escaped health hold", o)
			}
			continue
		}
		if o.ValuationSource != routeRefreshValuationSource || o.ValuationSlot != 78 || o.Snapshot.Slot != 78 || o.Snapshot.SquadsIdleRaw != 21 || o.Snapshot.CollateralIdleRaw != 3 || o.Snapshot.PositionCollateralRaw != 10 {
			t.Fatal("mixed bank or lost provenance", o)
		}
		route, _ := runtimeRoute(PhaseOneLaneID)
		navAccounts, err := selectRouteNAVAccountsForRoute(accounts, route)
		if err != nil {
			t.Fatal(err)
		}
		nav, err := ComputeRouteNAVForRoute(78, navAccounts, m, nil, route)
		if err != nil {
			t.Fatal(err)
		}
		if int64(nav.StrategyNAVRaw) != o.Snapshot.StrategyNAVRaw || nav.SnapshotDigest != o.Snapshot.ReportSnapshotDigest {
			t.Fatal("construction recomputation lost valuation provenance")
		}
		originalDigest := nav.SnapshotDigest
		for i := range navAccounts {
			navAccounts[i].ValuationSource = ""
			navAccounts[i].ValuationSlot = 0
		}
		confirmed, err := ComputeRouteNAVForRoute(78, navAccounts, m, nil, route)
		if err != nil {
			t.Fatal(err)
		}
		if confirmed.SnapshotDigest == originalDigest {
			t.Fatal("projected valuation aliases confirmed evidence")
		}
		navAccounts[0].ValuationSource = routeRefreshValuationSource
		navAccounts[0].ValuationSlot = 78
		if _, err := ComputeRouteNAVForRoute(78, navAccounts, m, nil, route); err == nil {
			t.Fatal("mixed provenance accepted")
		}
	}
}

func TestRouteValuationCaptureFreshnessRequiresIntegrity(t *testing.T) {
	const minimumSlot int64 = 77
	for _, lag := range []int64{0, observationLagSlots(), observationLagSlots() + 1} {
		slot := minimumSlot + lag
		accounts := []ConfirmedAccount{{Address: bridgeSquadsATA, ValuationSource: routeRefreshValuationSource, ValuationSlot: slot}}
		addresses := []string{bridgeSquadsATA}
		err := validateRouteValuationCapture(slot, accounts, addresses, minimumSlot)
		if lag <= observationLagSlots() {
			if err != nil {
				t.Fatal("fresh valid capture rejected", err)
			}
		} else if !errors.Is(err, errConfirmedObservationUnavailable) || !transientValuationRefreshFailure(err) {
			t.Fatal("late valid capture did not require a retry", err)
		}
		for _, fault := range []string{"missing", "extra", "namespace", "fee_payer", "duplicate", "source", "slot"} {
			copyAccounts := append([]ConfirmedAccount(nil), accounts...)
			copyAddresses := append([]string(nil), addresses...)
			switch fault {
			case "missing":
				copyAccounts = nil
			case "extra":
				copyAccounts = append(copyAccounts, accounts[0])
			case "namespace":
				copyAccounts[0].Address = kaminoDebtReserve
			case "fee_payer":
				copyAccounts[0].Address, copyAddresses[0] = bridgeDelegate, bridgeDelegate
			case "duplicate":
				copyAccounts = append(copyAccounts, accounts[0])
				copyAddresses = append(copyAddresses, addresses[0])
			case "source":
				copyAccounts[0].ValuationSource = ""
			case "slot":
				copyAccounts[0].ValuationSlot = slot - 1
			}
			if err := validateRouteValuationCapture(slot, copyAccounts, copyAddresses, minimumSlot); err == nil || transientValuationRefreshFailure(err) {
				t.Fatalf("invalid capture must fail hard even when late: lag=%d fault=%s err=%v", lag, fault, err)
			}
		}
	}
	accounts := []ConfirmedAccount{{Address: bridgeSquadsATA, ValuationSource: routeRefreshValuationSource, ValuationSlot: minimumSlot - 1}}
	if err := validateRouteValuationCapture(minimumSlot-1, accounts, []string{bridgeSquadsATA}, minimumSlot); err == nil || transientValuationRefreshFailure(err) {
		t.Fatal("regressed capture must fail hard", err)
	}
}

func TestSelectorValuationCaptureRetainsNAVAndOwnership(t *testing.T) {
	m := readyWorkerManifest(t)
	m.selectorObservation = true
	all := routeFixedAddresses(m)
	for _, lane := range selectorLanes {
		route, _ := runtimeRoute(lane)
		selected := selectorValuationAddresses(route, all)
		kept := map[string]bool{}
		for _, address := range selected {
			kept[address] = true
		}
		for _, address := range pinnedRouteNAVAddressesForRoute(route) {
			if !kept[address] {
				t.Fatal("lost active NAV", lane, address)
			}
		}
		for _, otherLane := range selectorLanes {
			other, _ := runtimeRoute(otherLane)
			if !kept[other.Kamino.Obligation] || !kept[other.CollateralCustody] {
				t.Fatal("lost lane ownership", lane, otherLane)
			}
		}
	}
}

// Archived pilot activation hashes include this exact pre-valuation account
// encoding. New optional metadata must not change old zero-source bytes.
func TestConfirmedAccountPreservesArchivedJSONEncoding(t *testing.T) {
	a := ConfirmedAccount{Address: "fixed", Owner: "owner", Lamports: 7, Data: []byte{1, 2}, Executable: false}
	b, err := json.Marshal(a)
	const archived = `{"Address":"fixed","Owner":"owner","Lamports":7,"Data":"AQI=","Executable":false}`
	if err != nil || string(b) != archived {
		t.Fatalf("archived activation encoding changed: %s %v", b, err)
	}
	a.ValuationSource = routeRefreshValuationSource
	a.ValuationSlot = 78
	b, err = json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var restored ConfirmedAccount
	if json.Unmarshal(b, &restored) != nil || restored.ValuationSource != a.ValuationSource || restored.ValuationSlot != 78 {
		t.Fatal("explicit provenance was dropped")
	}
}

// A reserve refresh that never reached the chain retries next tick; one that
// Kamino rejected still latches kamino_stale. Both start from stale reserves
// with noncash exposure.
func TestRouteRefreshTransientFailureRetriesInsteadOfLatching(t *testing.T) {
	for _, tc := range []struct {
		reason    string
		transient bool
	}{{"price_refresh_blockhash_unavailable", true}, {"price_refresh_simulation_unavailable", true}, {"price_refresh_simulation_failed", false}} {
		m := readyWorkerManifest(t)
		m.RuntimeActivation.SelectedLane = PhaseOneLaneID
		initial := productionRouteBatchAccounts(t, 77, func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint64(accountAt(a, budgetClockAddress).Data[:8], 77)
			binary.LittleEndian.PutUint64(accountAt(a, kaminoCollateralReserve).Data[16:24], 1)
			binary.LittleEndian.PutUint64(accountAt(a, kaminoDebtReserve).Data[16:24], 1)
		})
		o, _, err := observeConfirmedRouteSnapshotWithAccounts(context.Background(), m, routeObservationRuntime{
			read: fixtureBatchRuntime(77, initial),
			now:  func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
			refreshValuation: func(context.Context, RuntimeRoute, []string, int64) (int64, []ConfirmedAccount, error) {
				return 0, nil, budgetHold(tc.reason)
			},
		})
		if tc.transient {
			if !errors.Is(err, errConfirmedObservationUnavailable) || o.Snapshot.ManualReason != "" {
				t.Fatal("transient refresh failure latched or escaped retry", tc.reason, o.Snapshot.ManualReason, err)
			}
			continue
		}
		// A rejected refresh retries until the latch threshold (see
		// TestRejectedRefreshRetriesUntilThirdFailureInARow); pin the last one.
		if !errors.Is(err, errConfirmedObservationUnavailable) {
			t.Fatal("first rejected refresh did not retry", tc.reason, o.Snapshot.ManualReason, err)
		}
	}
	refreshSimulationFailures.Store(0)
}

func TestRouteRefreshAgedCaptureRetriesWithoutManualStop(t *testing.T) {
	t.Cleanup(func() {
		refreshSimulationFailures.Store(0)
		kaminoStaleHolds.Store(0)
	})
	m := readyWorkerManifest(t)
	m.RuntimeActivation.SelectedLane = PhaseOneLaneID
	initial := productionRouteBatchAccounts(t, 77, func(a []ConfirmedAccount) {
		binary.LittleEndian.PutUint64(accountAt(a, budgetClockAddress).Data[:8], 77)
		binary.LittleEndian.PutUint64(accountAt(a, kaminoCollateralReserve).Data[16:24], 1)
		binary.LittleEndian.PutUint64(accountAt(a, kaminoDebtReserve).Data[16:24], 1)
	})
	// Even near both latch thresholds, aged evidence must not count as bad health.
	refreshSimulationFailures.Store(refreshSimulationLatchAfter - 1)
	kaminoStaleHolds.Store(refreshSimulationLatchAfter - 1)
	for _, malformed := range []bool{false, true} {
		for i := 0; i <= refreshSimulationLatchAfter; i++ {
			o, accounts, err := observeConfirmedRouteSnapshotWithAccounts(context.Background(), m, routeObservationRuntime{
				read: fixtureBatchRuntime(77, initial),
				now:  func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
				refreshValuation: func(_ context.Context, _ RuntimeRoute, addresses []string, min int64) (int64, []ConfirmedAccount, error) {
					slot := min + observationLagSlots() + 1
					capture := make([]ConfirmedAccount, len(addresses))
					for j, address := range addresses {
						capture[j] = accountAt(initial, address)
						capture[j].Address = address
						capture[j].ValuationSource, capture[j].ValuationSlot = routeRefreshValuationSource, slot
					}
					if malformed {
						capture[0].ValuationSource = ""
					}
					// Match the production refresh boundary: reject before returning accounts.
					return 0, nil, validateRouteValuationCapture(slot, capture, addresses, min)
				},
			})
			if malformed {
				if err != nil || o.Snapshot.ManualReason != "kamino_stale" || o.Snapshot.Fresh {
					t.Fatal("mixed malformed/late capture escaped fail-closed hold", o.Snapshot, err)
				}
				break
			}
			if !errors.Is(err, errConfirmedObservationUnavailable) || o.Snapshot.ManualReason != "" || o.Snapshot.Fresh || len(accounts) != 0 {
				t.Fatal("aged capture latched or escaped retry", i, o.Snapshot, err)
			}
			if refreshSimulationFailures.Load() != refreshSimulationLatchAfter-1 || kaminoStaleHolds.Load() != refreshSimulationLatchAfter-1 {
				t.Fatal("aged capture changed a health failure streak")
			}
		}
	}
}

func TestRejectedRefreshRetriesUntilThirdFailureInARow(t *testing.T) {
	refreshSimulationFailures.Store(0)
	t.Cleanup(func() { refreshSimulationFailures.Store(0) })
	m := readyWorkerManifest(t)
	m.RuntimeActivation.SelectedLane = PhaseOneLaneID
	initial := productionRouteBatchAccounts(t, 77, func(a []ConfirmedAccount) {
		binary.LittleEndian.PutUint64(accountAt(a, budgetClockAddress).Data[:8], 77)
		binary.LittleEndian.PutUint64(accountAt(a, kaminoCollateralReserve).Data[16:24], 1)
		binary.LittleEndian.PutUint64(accountAt(a, kaminoDebtReserve).Data[16:24], 1)
	})
	rejected := true
	observe := func() (Observation, error) {
		o, _, err := observeConfirmedRouteSnapshotWithAccounts(context.Background(), m, routeObservationRuntime{
			read: fixtureBatchRuntime(77, initial),
			now:  func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
			refreshValuation: func(context.Context, RuntimeRoute, []string, int64) (int64, []ConfirmedAccount, error) {
				if rejected {
					return 0, nil, &BudgetHold{Reason: "price_refresh_simulation_failed", Details: map[string]string{"transactionError": `{"InstructionError":[0,{"Custom":6009}]}`}}
				}
				return 0, nil, context.DeadlineExceeded // transient: neither counts nor latches
			},
		})
		return o, err
	}
	for i := 1; i <= 2; i++ {
		if _, err := observe(); !errors.Is(err, errConfirmedObservationUnavailable) {
			t.Fatalf("rejected refresh %d latched instead of retrying: %v", i, err)
		}
	}
	o, err := observe()
	if err != nil || o.Snapshot.ManualReason != "kamino_stale" {
		t.Fatalf("third rejected refresh in a row did not hold kamino_stale: %v %+v", err, o.Snapshot.ManualReason)
	}
	// A refresh that reaches Kamino successfully resets the streak.
	refreshSimulationFailures.Store(2)
	rejected = false
	_, _ = observe()
	if refreshSimulationFailures.Load() != 2 {
		t.Fatal("a transient refresh failure changed the rejected streak")
	}
}
