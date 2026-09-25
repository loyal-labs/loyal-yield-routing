package backyardrwa

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestRouteRefreshValuesNoncashFromOneBankAndRejectsStaleOracle(t *testing.T) {
	for _, staleOracle := range []bool{false, true} {
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
		read, finalized := fixtureBatchRuntime(77, initial)
		calls := 0
		refreshCalls := 0
		o, accounts, err := observeConfirmedRouteSnapshotWithAccounts(context.Background(), m, routeObservationRuntime{
			confirmedSlot: func(context.Context) (int64, error) { return 77, nil },
			accounts: func(ctx context.Context, addresses []string, min int64) (int64, []ConfirmedAccount, error) {
				if min >= 78 {
					r, _ := fixtureBatchRuntime(78, captured)
					return r(ctx, addresses, min)
				}
				return read(ctx, addresses, min)
			},
			finalizedReceipt: finalized,
			receipts: func(context.Context, int64) (int64, []programAccount, error) {
				calls++
				if calls == 1 {
					return 77, nil, nil
				}
				return 78, nil, nil
			},
			now: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
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

func TestRouteValuationCaptureRejectsFeePayerAndMixedBank(t *testing.T) {
	accounts := []ConfirmedAccount{{Address: bridgeSquadsATA, ValuationSource: routeRefreshValuationSource, ValuationSlot: 78}}
	addresses := []string{bridgeSquadsATA}
	if err := validateRouteValuationCapture(78, accounts, addresses, 77); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func([]ConfirmedAccount){func(a []ConfirmedAccount) { a[0].ValuationSlot = 77 }, func(a []ConfirmedAccount) { a[0].ValuationSource = "" }, func(a []ConfirmedAccount) { a[0].Address = bridgeDelegate }} {
		copyAccounts := append([]ConfirmedAccount(nil), accounts...)
		change(copyAccounts)
		if err := validateRouteValuationCapture(78, copyAccounts, addresses, 77); err == nil {
			t.Fatal("invalid capture accepted")
		}
	}
}

func TestSelectorValuationCaptureRetainsNAVPoliciesAndOwnership(t *testing.T) {
	m := readyWorkerManifest(t)
	m.selectorObservation = true
	all := routeFixedAddresses(m)
	for _, lane := range selectorLanes {
		route, _ := runtimeRoute(lane)
		selected := selectorValuationPolicyAddresses(m, route, selectorValuationAddresses(route, all))
		kept := map[string]bool{}
		for _, address := range selected {
			kept[address] = true
		}
		for _, address := range pinnedRouteNAVAddressesForRoute(route) {
			if !kept[address] {
				t.Fatal("lost active NAV", lane, address)
			}
		}
		for _, binding := range m.RuntimeBindings.BridgePolicies {
			if !kept[binding.Account] {
				t.Fatal("lost bridge policy", lane, binding.Account)
			}
		}
		for _, family := range []BasicPolicyFamily{BasicCollateralLifecycle, BasicDebtLifecycle, BasicSwapRoutesA, BasicSwapRoutesB} {
			binding, _, err := m.basicPolicyBinding(family)
			if err == nil && !kept[binding.Policy] {
				t.Fatal("lost active basic policy", lane, binding.Policy)
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
		read, finalized := fixtureBatchRuntime(77, initial)
		o, _, err := observeConfirmedRouteSnapshotWithAccounts(context.Background(), m, routeObservationRuntime{
			confirmedSlot:    func(context.Context) (int64, error) { return 77, nil },
			accounts:         read,
			finalizedReceipt: finalized,
			receipts:         func(context.Context, int64) (int64, []programAccount, error) { return 77, nil, nil },
			now:              func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
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
	read, finalized := fixtureBatchRuntime(77, initial)
	rejected := true
	observe := func() (Observation, error) {
		o, _, err := observeConfirmedRouteSnapshotWithAccounts(context.Background(), m, routeObservationRuntime{
			confirmedSlot:    func(context.Context) (int64, error) { return 77, nil },
			accounts:         read,
			finalizedReceipt: finalized,
			receipts:         func(context.Context, int64) (int64, []programAccount, error) { return 77, nil, nil },
			now:              func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
			refreshValuation: func(context.Context, RuntimeRoute, []string, int64) (int64, []ConfirmedAccount, error) {
				if rejected {
					return 0, nil, &BudgetHold{Reason: "price_refresh_simulation_failed"}
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
