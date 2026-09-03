package backyardrwa

import "testing"

func TestPhase2SelectedLaneUsesRouteNeutralLifecycleActions(t *testing.T) {
	snapshot := Snapshot{
		ObservationID: "maple-state", Slot: 42, RouteKind: RouteKind, RouteLane: SelectedRouteID,
		StrategyKey: SelectedRouteID, Fresh: true, SquadsIdleRaw: 100,
		CollateralIdleRaw: 0, PolicyReady: true, ExitBuildable: true,
		CapacityRaw: 100, PolicyLimitRaw: 100, MaxTargetLTVEntryRaw: 100,
		LiquidationThresholdBPS: 9000,
	}
	decision := Decide(snapshot)
	if decision.Action != SwapStableToCollateralStep || decision.StrategyKey != SelectedRouteID {
		t.Fatalf("selected lane did not use stable-to-collateral action: %+v", decision)
	}

	snapshot.SquadsIdleRaw = 0
	snapshot.CollateralIdleRaw = 25
	decision = Decide(snapshot)
	if decision.Action != OpenRouteStep || decision.AmountRaw != 25 {
		t.Fatalf("selected lane did not open from collateral custody: %+v", decision)
	}
}

func TestPhase2UnsupportedLaneFailsClosed(t *testing.T) {
	decision := Decide(Snapshot{ObservationID: "other", Slot: 1, RouteKind: RouteKind, RouteLane: "OnRe/ONyc/USDC", Fresh: true})
	if decision.Action != HoldManualRecovery || decision.Reason != "unsupported_runtime_lane" {
		t.Fatalf("unsupported lane was not held: %+v", decision)
	}
}

func TestPhase2RuntimeActivationIsExactlyTwoRoutes(t *testing.T) {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.RuntimeActivation.SelectedLane != SelectedRouteID || len(manifest.RuntimeActivation.RuntimeRoutes) != RuntimeRouteCount ||
		manifest.RuntimeActivation.RuntimeRoutes[0] != PhaseOneLaneID || manifest.RuntimeActivation.RuntimeRoutes[1] != SelectedRouteID {
		t.Fatalf("unexpected runtime activation: %+v", manifest.RuntimeActivation)
	}
	route, err := manifest.activeRuntimeRoute()
	if err != nil || route.Lane != SelectedRouteID || route.Kamino.CollateralMint != "AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj" {
		t.Fatalf("selected route binding is not Maple/syrupUSDC/USDC: %+v, %v", route, err)
	}
}

func TestPhase2MapleKaminoPacketUsesPinnedGraph(t *testing.T) {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	request, err := manifest.kaminoPacketForRoute(OpenRouteStep, kaminoLegDeposit, 77, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 9}, SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	if request.RouteLane != SelectedRouteID || request.Policy != "5NyDUfvT3a5gKgh6KMn7qYi5Tp9YfCDUjiJYV1TsnX5c" || len(request.Accounts) != 17 {
		t.Fatalf("unexpected Maple Kamino packet: %+v", request)
	}
	if _, leg, err := kaminoRouteInstruction(request, SelectedRouteID); err != nil || leg != kaminoLegDeposit {
		t.Fatalf("Maple packet did not validate as deposit: %v, %v", err, leg)
	}
}

func TestPhase2CutoverRejectsAnyLegacyPrimeExposure(t *testing.T) {
	if legacyPrimeExposure(KaminoPosition{}, 0) {
		t.Fatal("flat legacy route was treated as exposed")
	}
	for _, test := range []struct {
		name     string
		position KaminoPosition
		custody  uint64
	}{
		{"custody", KaminoPosition{}, 1},
		{"position flag", KaminoPosition{HasPosition: true}, 0},
		{"collateral", KaminoPosition{CollateralDepositedRaw: 1}, 0},
		{"debt", KaminoPosition{DebtRaw: 1}, 0},
	} {
		if !legacyPrimeExposure(test.position, test.custody) {
			t.Fatalf("%s exposure was not rejected", test.name)
		}
	}
}

func TestPhase2JupiterBindingsUseDirectionSpecificInstalledPrefixes(t *testing.T) {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	entry, err := manifest.jupiterPolicyForRoute(SwapStableToCollateralStep, SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	exit, err := manifest.jupiterPolicyForRoute(SwapCollateralToStableStep, SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.ConstraintBindings[0].RoutePlanPrefixHex != "01010000007400640001" ||
		exit.ConstraintBindings[0].RoutePlanPrefixHex != "02010000007400640001" {
		t.Fatalf("unexpected Phase 2 route prefixes: entry=%+v exit=%+v", entry, exit)
	}
}

func TestPhase2DecisionIsClampedToAuthorizedPerTransactionCap(t *testing.T) {
	decision := Decide(Snapshot{
		ObservationID: "cap", Slot: 1, RouteKind: RouteKind, RouteLane: SelectedRouteID,
		Fresh: true, VoltrIdleRaw: Phase2TransactionCapRaw + 1,
	})
	if decision.Action != VoltrAllocateToSquads || decision.AmountRaw != Phase2TransactionCapRaw {
		t.Fatalf("selected route decision exceeded cap: %+v", decision)
	}
}

func TestRouteNeutralActionRequiresSelectedStrategy(t *testing.T) {
	decision := Decision{Action: OpenRouteStep, Reason: "test", AmountRaw: 1, IdempotencyKey: "test"}
	if decision.Validate() == nil {
		t.Fatal("route-neutral action without selected strategy was accepted")
	}
	decision.StrategyKey = SelectedRouteID
	if err := decision.Validate(); err != nil {
		t.Fatalf("selected route-neutral action was rejected: %v", err)
	}
}

func TestPhase2CutoverDrainChunksLegacyCollateral(t *testing.T) {
	decision := Decide(Snapshot{
		ObservationID: "cutover", Slot: 1, RouteKind: RouteKind, RouteLane: RouteID,
		Fresh: true, CutoverDrain: true, HasPosition: true,
		PositionCollateralRaw:   Phase2TransactionCapRaw + 500_000,
		StrategyNAVRaw:          Phase2TransactionCapRaw + 500_000,
		LiquidationThresholdBPS: 9000,
	})
	if decision.Action != DeleverPrimeUSDCStep || decision.AmountRaw != Phase2TransactionCapRaw {
		t.Fatalf("legacy cutover was not chunked: %+v", decision)
	}
	leg, receiptRaw, collateralRaw, err := selectKaminoLeg(decision, KaminoPosition{
		HasPosition: true, CollateralDepositedRaw: 1_500_000, RedeemablePrimeRaw: 1_200_000,
	})
	if err != nil || leg != kaminoLegWithdraw || receiptRaw != 1_000_000 || collateralRaw != 800_000 {
		t.Fatalf("partial legacy withdrawal is wrong: leg=%v receipt=%d collateral=%d err=%v", leg, receiptRaw, collateralRaw, err)
	}
}
