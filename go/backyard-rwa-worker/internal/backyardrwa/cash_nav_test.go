package backyardrwa

import (
	"context"
	"encoding/binary"
	"testing"
)

func TestCashNAVDoesNotDependOnEntryMarket(t *testing.T) {
	const slot = int64(77)
	manifest := readyWorkerManifest(t)
	route, _ := runtimeRoute(RouteID)
	flat := func() []ConfirmedAccount {
		accounts := routeNAVFixture(t, slot)
		clear(accountAt(accounts, route.Kamino.Obligation).Data[96:2208])
		binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 0)
		binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.CollateralReserve).Data[16:24], 1)
		return accounts
	}
	for _, kind := range []string{"stale", "paused", "emergency", "zero_price"} {
		t.Run(kind, func(t *testing.T) {
			accounts := flat()
			switch kind {
			case "paused":
				accountAt(accounts, route.Kamino.CollateralReserve).Data[kaminoReserveStatusOffset] = 1
			case "emergency":
				accountAt(accounts, route.Kamino.Market).Data[kaminoMarketEmergencyModeOffset] = 1
			case "zero_price":
				clear(accountAt(accounts, route.Kamino.CollateralReserve).Data[248:264])
			}
			nav, err := ComputeRouteNAV(slot, accounts, manifest, nil)
			if err != nil || nav.StrategyNAVRaw != 6 || nav.TotalVaultNAVRaw != 17 {
				t.Fatalf("cash accounting: %+v %v", nav, err)
			}
			reader := func(context.Context, []string, int64) (int64, []ConfirmedAccount, error) {
				t.Fatal("unavailable market must not fetch oracles for cash accounting")
				return 0, nil, nil
			}
			p, err := observeKaminoWithCashFallback(context.Background(), reader, slot, accounts, route)
			if err != nil || p.HasPosition || p.EntryCapacityRaw != 0 || !p.BorrowUtilizationBlocked {
				t.Fatalf("cash observation opened entry: %+v %v", p, err)
			}
			s := base()
			s.RouteLane, s.StrategyKey = PhaseOneLaneID, PhaseOneLaneID
			s.SquadsIdleRaw, s.CapacityRaw, s.MaxTargetLTVEntryRaw = int64(nav.Custodies.SquadsUSDCraw), int64(p.EntryCapacityRaw), int64(p.EntryCapacityRaw)
			s.LiquidationThresholdBPS = p.LiquidationThresholdBPS
			s.ObligationPresenceKnown, s.ObligationPresent = true, p.ObligationPresent
			for _, present := range []bool{true, false} {
				s.ObligationPresent = present
				if d := Decide(s); d.Action != StageSquadsToVoltr || d.AmountRaw != s.SquadsIdleRaw {
					t.Fatalf("unavailable entry market trapped cash: %+v", d)
				}
			}
		})
	}
	for _, kind := range []string{"custody", "collateral", "debt", "future_slot", "foreign_custody", "override_hides_exposure", "override_adds_exposure"} {
		t.Run(kind, func(t *testing.T) {
			accounts := flat()
			var override *RouteNAVCustodies
			switch kind {
			case "custody", "override_hides_exposure":
				binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 1)
				if kind == "override_hides_exposure" {
					override = &RouteNAVCustodies{SquadsUSDCraw: 6}
				}
			case "collateral", "debt":
				collateral, debt := uint64(1), uint64(0)
				if kind == "debt" {
					collateral, debt = 0, 1
				}
				copy(accountAt(accounts, route.Kamino.Obligation).Data, obligationFixture(t, slot, collateral, debt).Data)
			case "future_slot":
				binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.CollateralReserve).Data[16:24], uint64(slot+1))
			case "foreign_custody":
				putKey(t, accountAt(accounts, route.CollateralCustody).Data[32:64], bridgeDelegate)
			case "override_adds_exposure":
				override = &RouteNAVCustodies{SquadsUSDCraw: 6, SquadsPRIMEraw: 1}
			}
			if _, err := ComputeRouteNAV(slot, accounts, manifest, override); err == nil {
				t.Fatal("unsafe NAV accepted")
			}
			if override == nil {
				reader := func(context.Context, []string, int64) (int64, []ConfirmedAccount, error) {
					t.Fatal("unexpected oracle fetch")
					return 0, nil, nil
				}
				if _, err := observeKaminoWithCashFallback(context.Background(), reader, slot, accounts, route); err == nil {
					t.Fatal("unsafe cash fallback accepted")
				}
			}
		})
	}
}
