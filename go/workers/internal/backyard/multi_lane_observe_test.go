package backyard

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"
)

// multiLaneFixture is the coherent AUTO candidate batch observed in selector
// mode: every other registry lane joins with an absent obligation and empty
// custodies, exactly as the production inventory requests them, and mutate
// may then fund further lanes. The manifest prefers AUTO.
func multiLaneFixture(t *testing.T, mutate func(*[]ConfirmedAccount)) (RouteManifest, []ConfirmedAccount) {
	t.Helper()
	manifest, _, accounts := autoObservationBatch(t, 77, nil)
	manifest.selectorObservation = true
	manifest.observationLane = autoAUTOPYUSD.Lane
	for _, lane := range earnLaneIDs(true) {
		if lane == autoAUTOPYUSD.Lane {
			continue
		}
		route, err := runtimeRoute(lane)
		if err != nil {
			t.Fatal(err)
		}
		upsertConfirmedAccount(&accounts, ConfirmedAccount{Address: route.Kamino.Obligation})
		if accountAt(accounts, route.CollateralCustody).Address == "" {
			custody := tokenAccountFixture(t, route.CollateralCustody, route.Kamino.CollateralMint, bridgeVault, 0)
			custody.Owner = route.CollateralTokenProgram
			upsertConfirmedAccount(&accounts, custody)
		}
		if route.Kamino.DebtMint != bridgeUSDC && accountAt(accounts, route.DebtCustody).Address == "" {
			custody := tokenAccountFixture(t, route.DebtCustody, route.Kamino.DebtMint, bridgeVault, 0)
			custody.Owner = route.DebtTokenProgram
			upsertConfirmedAccount(&accounts, custody)
		}
	}
	if mutate != nil {
		mutate(&accounts)
	}
	return manifest, accounts
}

// fundLane gives lane a position of collateralRaw receipts (redeeming 1:1,
// six decimals, priced at par) against debtRaw debt, with fresh reserves and
// market in the same batch.
func fundLane(t *testing.T, accounts *[]ConfirmedAccount, lane string, collateralRaw, debtRaw uint64) RuntimeRoute {
	t.Helper()
	route, err := runtimeRoute(lane)
	if err != nil {
		t.Fatal(err)
	}
	peg := new(big.Int).Lsh(big.NewInt(1), 60)
	upsertConfirmedAccount(accounts, kaminoReserveImage(t, route.Kamino.Market, route.Kamino.CollateralReserve, route.Kamino.CollateralMint, 77, peg, 1_000_000_000, 0, 1_000_000_000, 6))
	upsertConfirmedAccount(accounts, kaminoReserveImage(t, route.Kamino.Market, route.Kamino.DebtReserve, route.Kamino.DebtMint, 77, peg, 1_000_000_000-debtRaw, debtRaw, 1_000_000_000, 6))
	upsertConfirmedAccount(accounts, marketFixture(t, route.Kamino.Market))
	upsertConfirmedAccount(accounts, kaminoObligationImage(t, route, 77, collateralRaw, debtRaw))
	return route
}

func observeMultiLane(t *testing.T, manifest RouteManifest, accounts []ConfirmedAccount) Observation {
	t.Helper()
	o, _, err := observeConfirmedRouteSnapshotWithAccounts(context.Background(), manifest, routeObservationRuntime{
		read: fixtureBatchRuntime(77, accounts),
		now:  func() time.Time { return time.Unix(kaminoFixtureUnix, 0).UTC() },
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.Snapshot.ManualReason != "" || !o.Snapshot.MonitorsArmed {
		t.Fatalf("multi-lane batch did not observe armed: %q %+v", o.Snapshot.ManualReason, o.Snapshot)
	}
	return o
}

// One held lane keeps the exact one-lane book: the vault NAV, the report
// digest and the observation identity of the one-lane observer, which the
// fixture's figures pin (32_000_010 = Squads 6 + idle collateral 4 +
// 30_000_000 collateral - 7_000_000 PYUSD debt + 9_000_000 idle PYUSD).
func TestOneHeldLaneNAVIsTheOneLaneNAV(t *testing.T) {
	manifest, accounts := multiLaneFixture(t, nil)
	o := observeMultiLane(t, manifest, accounts)
	s := o.Snapshot
	if s.RouteLane != autoAUTOPYUSD.Lane || s.HeldLanes != autoAUTOPYUSD.Lane || s.OtherLanesNAVRaw != 0 || s.CrossLaneHardLTV {
		t.Fatalf("one held lane is not the working lane alone: %q %q %d", s.RouteLane, s.HeldLanes, s.OtherLanesNAVRaw)
	}
	if s.StrategyNAVRaw != 32_000_010 || s.TotalVaultNAVRaw != 32_000_021 {
		t.Fatalf("one-lane NAV drifted: %d/%d", s.StrategyNAVRaw, s.TotalVaultNAVRaw)
	}
	// The same batch through the single-route calculator gives the same
	// report bytes: the union namespace of one lane is its own namespace.
	navAccounts, err := selectRouteNAVAccountsForRoute(accounts, autoAUTOPYUSD)
	if err != nil {
		t.Fatal(err)
	}
	single, err := ComputeRouteNAVForRoute(77, navAccounts, manifest, nil, autoAUTOPYUSD)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(s.StrategyNAVRaw) != single.StrategyNAVRaw || s.ReportSnapshotDigest != single.SnapshotDigest {
		t.Fatalf("one held lane changed the report: %d %s vs %d %s", s.StrategyNAVRaw, s.ReportSnapshotDigest, single.StrategyNAVRaw, single.SnapshotDigest)
	}
	// The observation identity is the one-lane identity (pinned from the
	// one-lane observer on the same fixture).
	if s.ObservationID != oneLaneAUTOObservationID || s.ReportSnapshotDigest != oneLaneAUTOReportDigest {
		t.Fatalf("one-lane observation identity or report digest changed: %s %s", s.ObservationID, s.ReportSnapshotDigest)
	}
}

// The observation identity and report digest the one-lane observer (before
// multi-lane observation, origin/main 877b297e) gave this test's batch.
const (
	oneLaneAUTOObservationID = "d2f0257fb3e4fb86c63671447f66162d942a5dd73f63d0c1cc0f04b118509515"
	oneLaneAUTOReportDigest  = "0bf7ffa2e7e01686c2425c13b71b7447aa284c4246ad96a4b06d5e9dc1b1f619"
)

// Two held lanes are one book: the working lane's view plus every other held
// lane's obligation, with the Squads USDC ATA (OnRe's debt custody) counted
// once. OnRe adds 10_000_000 collateral - 2_000_000 USDC debt.
func TestTwoHeldLanesNAVCountsEverySharedCustodyOnce(t *testing.T) {
	manifest, accounts := multiLaneFixture(t, func(accounts *[]ConfirmedAccount) {
		fundLane(t, accounts, onreONycUSDC, 10_000_000, 2_000_000)
	})
	s := observeMultiLane(t, manifest, accounts).Snapshot
	if s.RouteLane != autoAUTOPYUSD.Lane || s.HeldLanes != onreONycUSDC+","+autoAUTOPYUSD.Lane || s.CrossLaneHardLTV {
		t.Fatalf("held set or working lane wrong: %q %q", s.RouteLane, s.HeldLanes)
	}
	if s.OtherLanesNAVRaw != 8_000_000 || s.StrategyNAVRaw != 32_000_010+8_000_000 || s.TotalVaultNAVRaw != 32_000_021+8_000_000 {
		t.Fatalf("two-lane NAV: other %d strategy %d total %d", s.OtherLanesNAVRaw, s.StrategyNAVRaw, s.TotalVaultNAVRaw)
	}
	// The working lane's per-lane view is unchanged by the other lane.
	if s.PositionCollateralValueRaw != 30_000_000 || s.PositionDebtValueRaw != 7_000_000 || s.SquadsIdleRaw != 6 || s.DebtIdleRaw != int64(autoFixtureDebtIdleRaw) {
		t.Fatalf("working-lane view absorbed another lane: %+v", s)
	}
	if s.ObservationID == oneLaneAUTOObservationID {
		t.Fatal("another held lane left the observation identity unchanged")
	}

	// AUTO and Prime/PYUSD share the PYUSD custody: its 9_000_000 idle PYUSD
	// is the working lane's, counted once. Prime/PYUSD adds 5_000_000 -
	// 1_000_000.
	manifest, accounts = multiLaneFixture(t, func(accounts *[]ConfirmedAccount) {
		fundLane(t, accounts, primePRIMEPYUSD.Lane, 5_000_000, 1_000_000)
	})
	s = observeMultiLane(t, manifest, accounts).Snapshot
	if s.HeldLanes != autoAUTOPYUSD.Lane+","+primePRIMEPYUSD.Lane || s.RouteLane != autoAUTOPYUSD.Lane {
		t.Fatalf("PYUSD-sharing held set wrong: %q %q", s.RouteLane, s.HeldLanes)
	}
	if s.OtherLanesNAVRaw != 4_000_000 || s.StrategyNAVRaw != 32_000_010+4_000_000 {
		t.Fatalf("shared PYUSD custody counted twice or lost: other %d strategy %d", s.OtherLanesNAVRaw, s.StrategyNAVRaw)
	}
	// The bridge report built for this observation values the same book.
	routes, err := navRoutes(autoAUTOPYUSD, s.heldLanes())
	if err != nil {
		t.Fatal(err)
	}
	navAccounts, err := selectRouteNAVAccountsForRoutes(accounts, routes)
	if err != nil {
		t.Fatal(err)
	}
	nav, err := ComputeRouteNAVForRoutes(77, navAccounts, manifest, nil, routes)
	if err != nil || nav.Report.NAVAfterRaw != uint64(s.StrategyNAVRaw) {
		t.Fatalf("report NAV is not the held book: %d %v", nav.Report.NAVAfterRaw, err)
	}
}

// A held lane past its hard LTV is handled before the working lane: the
// per-lane view moves to it and Decide's risk branch repays it.
func TestHeldLanePastHardLTVIsProtectedFirst(t *testing.T) {
	manifest, accounts := multiLaneFixture(t, func(accounts *[]ConfirmedAccount) {
		// 80% liquidation threshold: hard LTV 6000; 7_000_000/10_000_000 = 70%.
		fundLane(t, accounts, onreONycUSDC, 10_000_000, 7_000_000)
	})
	s := observeMultiLane(t, manifest, accounts).Snapshot
	if s.RouteLane != onreONycUSDC || !s.CrossLaneHardLTV || s.LTVBPS < 6000 {
		t.Fatalf("risky held lane was not protected first: %q cross=%t ltv=%d", s.RouteLane, s.CrossLaneHardLTV, s.LTVBPS)
	}
	// The bridge monitors read journal facts this batch-only fixture lacks;
	// the risk branch itself is the per-lane Decide.
	s.MonitorsArmed = false
	d := Decide(s)
	if d.StrategyKey != onreONycUSDC || !strings.HasPrefix(d.Reason, "hard_ltv_") {
		t.Fatalf("risky held lane did not get the hard-LTV decision: %+v", d)
	}
}

// A shared custody balance belongs to the one positioned sharing lane; two
// positioned sharing lanes with neither working nor preferred among them is
// the one unattributable case, refused.
func TestSharedCustodyBalanceAttribution(t *testing.T) {
	_, accounts := multiLaneFixture(t, func(accounts *[]ConfirmedAccount) {
		fundLane(t, accounts, primePRIMEPYUSD.Lane, 5_000_000, 1_000_000)
		upsertConfirmedAccount(accounts, tokenAccountFixture(t, kaminoPrimeCustody, kaminoPrimeMint, bridgeVault, 1))
	})
	working, held, err := observedSelectorLanes(accounts, autoAUTOPYUSD.Lane, "")
	if err != nil || working.Lane != autoAUTOPYUSD.Lane || strings.Join(held, ",") != autoAUTOPYUSD.Lane+","+primePRIMEPYUSD.Lane {
		t.Fatalf("PRIME balance beside one positioned Prime lane: %s %v %v", working.Lane, held, err)
	}
	route, _ := runtimeRoute(PhaseOneLaneID)
	upsertConfirmedAccount(&accounts, kaminoObligationImage(t, route, 77, 1_000_000, 0))
	if _, _, err = observedSelectorLanes(accounts, autoAUTOPYUSD.Lane, ""); err == nil || err.Error() != "selector_shared_custody_unattributable" {
		t.Fatalf("PRIME balance beside two positioned Prime lanes was attributed: %v", err)
	}
	if working, _, err = observedSelectorLanes(accounts, autoAUTOPYUSD.Lane, PhaseOneLaneID); err != nil || working.Lane != PhaseOneLaneID {
		t.Fatalf("the working Prime lane owns its transient PRIME: %s %v", working.Lane, err)
	}
}
