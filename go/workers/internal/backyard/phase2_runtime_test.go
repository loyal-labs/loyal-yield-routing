package backyard

import (
	"testing"
)

// embeddedTestManifest is the manifest the worker runs.
func embeddedTestManifest(t testing.TB) RouteManifest {
	t.Helper()
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestPhase2SelectedLaneUsesRouteNeutralLifecycleActions(t *testing.T) {
	t.Parallel()
	snapshot := Snapshot{
		ObservationID: "maple-state", Slot: 42, RouteKind: RouteKind, RouteLane: MapleSyrupUSDCLaneID,
		StrategyKey: MapleSyrupUSDCLaneID, Fresh: true, SquadsIdleRaw: 100,
		CollateralIdleRaw: 0, MinimumCollateralDepositRaw: 1, CapacityRaw: 100, PolicyLimitRaw: 100, MaxTargetLTVEntryRaw: 100,
		LiquidationThresholdBPS: 9000, MaxLTVBPS: 9000,
		// The selector's entry into this lane already allocated the cash.
		Target: LaneTarget{Rule: ruleEntry},
	}
	decision := Decide(snapshot)
	if decision.Action != SwapStableToCollateralStep || decision.StrategyKey != MapleSyrupUSDCLaneID {
		t.Fatalf("selected lane did not use stable-to-collateral action: %+v", decision)
	}

	snapshot.SquadsIdleRaw = 0
	snapshot.CollateralIdleRaw = 25_000
	decision = Decide(snapshot)
	if decision.Action != OpenRouteStep || decision.AmountRaw != 25_000 {
		t.Fatalf("selected lane did not open from collateral custody: %+v", decision)
	}
}

func TestPhase2WithdrawalDemandKeepsTerminalIdleCovered(t *testing.T) {
	t.Parallel()
	snapshot := Snapshot{
		ObservationID: "maple-terminal", Slot: 43, RouteKind: RouteKind,
		RouteLane: MapleSyrupUSDCLaneID, StrategyKey: MapleSyrupUSDCLaneID, Fresh: true,
		WithdrawalDemandRaw: 1, StrategyNAVRaw: 2_793_180,
		VoltrIdleRaw: 2_793_180, CollateralIdleRaw: 0,
		LiquidationThresholdBPS: 8_000, MaxLTVBPS: 8_000}

	decision := Decide(snapshot)
	if decision.Action != Hold || decision.Reason != string(holdAtTarget) || decision.AmountRaw != 0 {
		t.Fatalf("selected-lane terminal withdrawal state is not stable: %+v", decision)
	}
}

func TestPhase2UnsupportedLaneFailsClosed(t *testing.T) {
	t.Parallel()
	if _, err := runtimeRoute("OnRe/ONyc/USDC"); err != nil {
		t.Fatal(err)
	}
	// The OnRe sibling shares the phase 3 budget family but no runtime
	// install, so it must hold exactly like a fully unknown lane.
	for _, lane := range []string{"unknown/asset/debt", "OnRe/ONyc/USDG"} {
		decision := Decide(Snapshot{ObservationID: "other", Slot: 1, RouteKind: RouteKind, RouteLane: lane, Fresh: true})
		if decision.Action != HoldManualRecovery || decision.Reason != "unsupported_runtime_lane" || decision.StrategyKey != lane {
			t.Fatalf("unsupported lane was not held: %+v", decision)
		}
	}
}

func TestPhase2MapleKaminoPacketUsesPinnedGraph(t *testing.T) {
	t.Parallel()
	manifest := embeddedTestManifest(t)
	var err error
	request, err := manifest.kaminoPacketForRoute(testPolicies(t), OpenRouteStep, kaminoLegDeposit, 77, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 9}, MapleSyrupUSDCLaneID)
	if err != nil {
		t.Fatal(err)
	}
	if request.RouteLane != MapleSyrupUSDCLaneID || request.Policy != mustBasicPolicy(t, BasicCollateralLifecycle) || len(request.Accounts) != 17 {
		t.Fatalf("unexpected Maple Kamino packet: %+v", request)
	}
	if _, leg, err := kaminoRouteInstruction(rwa, request, MapleSyrupUSDCLaneID); err != nil || leg != kaminoLegDeposit {
		t.Fatalf("Maple packet did not validate as deposit: %v, %v", err, leg)
	}
	borrow, err := manifest.kaminoPacketForRoute(testPolicies(t), OpenRouteStep, kaminoLegBorrow, 77, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 9}, MapleSyrupUSDCLaneID)
	if err != nil {
		t.Fatal(err)
	}
	if borrow.Policy != mustBasicPolicy(t, BasicDebtLifecycle) || borrow.Accounts[12].Address != mapleSyrupUSDCUSDC.ObligationDebtFarm || borrow.Accounts[13].Address != mapleSyrupUSDCUSDC.DebtFarm {
		t.Fatalf("borrow packet did not pin live farm accounts: %+v", borrow)
	}
	repay, err := manifest.kaminoPacketForRoute(testPolicies(t), DeleverRouteStep, kaminoLegRepay, 77, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 9}, MapleSyrupUSDCLaneID)
	if err != nil {
		t.Fatal(err)
	}
	if repay.Policy != mustBasicPolicy(t, BasicDebtLifecycle) || repay.Accounts[9].Address != mapleSyrupUSDCUSDC.ObligationDebtFarm || repay.Accounts[10].Address != mapleSyrupUSDCUSDC.DebtFarm {
		t.Fatalf("repay packet did not pin live farm accounts: %+v", repay)
	}
}

func TestPhase2BasicFamilyBindingsCoverAllRuntimeLanes(t *testing.T) {
	t.Parallel()
	manifest := embeddedTestManifest(t)
	for _, lane := range []string{PrimeUSDCLaneID, MapleSyrupUSDCLaneID, "OnRe/ONyc/USDC"} {
		route, err := runtimeRoute(lane)
		if err != nil || !basicLane(route.Lane) {
			t.Fatalf("%s is not a basic runtime route: %+v, %v", lane, route, err)
		}
		for _, test := range []struct {
			leg    kaminoLeg
			action Action
			family BasicPolicyFamily
		}{
			{kaminoLegDeposit, OpenRouteStep, BasicCollateralLifecycle},
			{kaminoLegWithdraw, DeleverRouteStep, BasicCollateralLifecycle},
			{kaminoLegBorrow, OpenRouteStep, BasicDebtLifecycle},
			{kaminoLegRepay, DeleverRouteStep, BasicDebtLifecycle},
		} {
			request, err := manifest.kaminoPacketForRoute(testPolicies(t), test.action, test.leg, 77, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 9}, lane)
			if err != nil {
				t.Fatalf("%s %v: %v", lane, test.leg, err)
			}
			if request.Policy != mustBasicPolicy(t, test.family) {
				t.Fatalf("%s %v resolved to policy=%s", lane, test.leg, request.Policy)
			}
			if _, got, err := kaminoRouteInstruction(rwa, request, lane); err != nil || got != test.leg {
				t.Fatalf("%s %v did not validate: %v, %v", lane, test.leg, err, got)
			}
		}
		for _, test := range []struct {
			action Action
			family BasicPolicyFamily
		}{
			{SwapStableToCollateralStep, BasicSwapRoutesA},
			{SwapCollateralToStableStep, BasicSwapRoutesB},
		} {
			key, leg, err := jupiterPolicyLeg(rwa, lane, test.action)
			if err != nil {
				t.Fatalf("%s %s: %v", lane, test.action, err)
			}
			wantLeg := byte(basicSwapONycPrime)
			if lane == MapleSyrupUSDCLaneID {
				wantLeg = basicSwapSyrup
			}
			if key != (policyKey{family: test.family}) || leg != wantLeg {
				t.Fatalf("%s %s resolved to policy=%s leg=%d", lane, test.action, key, leg)
			}
		}
	}
}

func mustBasicPolicy(t *testing.T, family BasicPolicyFamily) string {
	t.Helper()
	return testPolicyAccount(policyKey{family: family})
}

func TestRouteNeutralActionRequiresSelectedStrategy(t *testing.T) {
	t.Parallel()
	decision := Decision{Action: OpenRouteStep, Reason: "test", AmountRaw: 1, IdempotencyKey: "test"}
	if decision.Validate() == nil {
		t.Fatal("route-neutral action without selected strategy was accepted")
	}
	decision.StrategyKey = MapleSyrupUSDCLaneID
	if err := decision.Validate(); err != nil {
		t.Fatalf("selected route-neutral action was rejected: %v", err)
	}
}
