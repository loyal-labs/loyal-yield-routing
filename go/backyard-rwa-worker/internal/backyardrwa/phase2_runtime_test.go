package backyardrwa

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
)

func basicPolicyFixtureManifest(t *testing.T) RouteManifest {
	t.Helper()
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	hashes := map[BasicPolicyFamily]*string{}
	for index, family := range []BasicPolicyFamily{BasicCollateralLifecycle, BasicDebtLifecycle, BasicSwapRoutesA, BasicSwapRoutesB} {
		hash := strings.Repeat(string("abcd"[index:index+1]), 64)
		hashes[family] = &hash
	}
	manifest.RuntimeBindings.CollateralLifecycle.DataSHA256 = hashes[BasicCollateralLifecycle]
	manifest.RuntimeBindings.DebtLifecycle.DataSHA256 = hashes[BasicDebtLifecycle]
	manifest.RuntimeBindings.SwapRoutesA.DataSHA256 = hashes[BasicSwapRoutesA]
	manifest.RuntimeBindings.SwapRoutesB.DataSHA256 = hashes[BasicSwapRoutesB]
	return manifest
}

func TestPhase2SelectedLaneUsesRouteNeutralLifecycleActions(t *testing.T) {
	snapshot := Snapshot{
		ObservationID: "maple-state", Slot: 42, RouteKind: RouteKind, RouteLane: SelectedRouteID,
		StrategyKey: SelectedRouteID, Fresh: true, SquadsIdleRaw: 100,
		CollateralIdleRaw: 0, MinimumCollateralDepositRaw: 1, PolicyReady: true, ExitBuildable: true,
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

func TestPhase2WithdrawalDemandDrainsEntireSelectedLane(t *testing.T) {
	snapshot := Snapshot{
		ObservationID: "maple-withdrawal", Slot: 42, RouteKind: RouteKind,
		RouteLane: SelectedRouteID, StrategyKey: SelectedRouteID, Fresh: true,
		WithdrawalDemandRaw: 1, StrategyNAVRaw: 2_793_180,
		HasPosition: true, PositionCollateralRaw: 3_000_000,
		PositionDebtRaw: 590_717, CollateralIdleRaw: 250_000,
		LiquidationThresholdBPS: 8_000, LTVBPS: 2_000,
		PolicyReady: true, ExitBuildable: true,
	}

	decision := Decide(snapshot)
	// This buffer is not proven to cover all debt plus interest; release more
	// collateral before quoting a bounded full-payoff funding swap.
	if decision.Action != DeleverRouteStep || decision.Reason != "withdrawal_release_repayment_collateral" || decision.AmountRaw != 1 {
		t.Fatalf("selected-lane withdrawal did not begin full conservative drain: %+v", decision)
	}
	if decision.StrategyKey != SelectedRouteID {
		t.Fatalf("selected-lane withdrawal lost strategy binding: %+v", decision)
	}
}

func TestPhase2WithdrawalDemandKeepsTerminalIdleCovered(t *testing.T) {
	snapshot := Snapshot{
		ObservationID: "maple-terminal", Slot: 43, RouteKind: RouteKind,
		RouteLane: SelectedRouteID, StrategyKey: SelectedRouteID, Fresh: true,
		WithdrawalDemandRaw: 1, StrategyNAVRaw: 2_793_180,
		VoltrIdleRaw: 2_793_180, CollateralIdleRaw: 0,
		LiquidationThresholdBPS: 8_000, PolicyReady: true, ExitBuildable: true,
	}

	decision := Decide(snapshot)
	if decision.Action != Hold || decision.Reason != "withdrawal_covered" || decision.AmountRaw != 0 {
		t.Fatalf("selected-lane terminal withdrawal state is not stable: %+v", decision)
	}
}

func TestPhase2PinnedRuntimeAddressesAreCanonicalBase58(t *testing.T) {
	manifest := basicPolicyFixtureManifest(t)
	addresses := routeFixedAddresses(manifest)
	for _, lane := range []string{RouteID, SelectedRouteID} {
		route, err := runtimeRoute(lane)
		if err != nil {
			t.Fatal(err)
		}
		addresses = append(addresses,
			route.Kamino.Program, route.Kamino.Market, route.Kamino.Obligation,
			route.Kamino.CollateralReserve, route.Kamino.DebtReserve,
			route.Kamino.Vault, route.Kamino.MarketAuthority,
			route.Kamino.CollateralMint, route.Kamino.DebtMint,
			route.CollateralCustody, route.DebtCustody,
		)
	}
	for _, address := range addresses {
		if _, err := decodeBase58PublicKey(address); err != nil {
			t.Errorf("non-canonical pinned runtime address %q: %v", address, err)
		}
	}
}

func TestPhase2UnsupportedLaneFailsClosed(t *testing.T) {
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

func TestPhase2RuntimeActivationIncludesBasicRoutes(t *testing.T) {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.RuntimeActivation.SelectedLane != SelectedRouteID || len(manifest.RuntimeActivation.RuntimeRoutes) != RuntimeRouteCount ||
		manifest.RuntimeActivation.RuntimeRoutes[0].Lane != PhaseOneLaneID || manifest.RuntimeActivation.RuntimeRoutes[1].Lane != SelectedRouteID ||
		manifest.RuntimeActivation.RuntimeRoutes[2].Lane != "OnRe/ONyc/USDC" {
		t.Fatalf("unexpected runtime activation: %+v", manifest.RuntimeActivation)
	}
	route, err := manifest.activeRuntimeRoute()
	if err != nil || route.Lane != SelectedRouteID || route.Kamino.CollateralMint != "AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj" {
		t.Fatalf("selected route binding is not Maple/syrupUSDC/USDC: %+v, %v", route, err)
	}
}

func TestPhase2MapleKaminoPacketUsesPinnedGraph(t *testing.T) {
	manifest := basicPolicyFixtureManifest(t)
	var err error
	request, err := manifest.kaminoPacketForRoute(OpenRouteStep, kaminoLegDeposit, 77, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 9}, SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	if request.RouteLane != SelectedRouteID || request.Policy != mustBasicPolicy(t, BasicCollateralLifecycle).Policy || request.PolicyConstraintIndex != 0 || len(request.Accounts) != 17 {
		t.Fatalf("unexpected Maple Kamino packet: %+v", request)
	}
	if _, leg, err := kaminoRouteInstruction(request, SelectedRouteID); err != nil || leg != kaminoLegDeposit {
		t.Fatalf("Maple packet did not validate as deposit: %v, %v", err, leg)
	}
	borrow, err := manifest.kaminoPacketForRoute(OpenRouteStep, kaminoLegBorrow, 77, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 9}, SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	if borrow.Policy != mustBasicPolicy(t, BasicDebtLifecycle).Policy || borrow.PolicyConstraintIndex != 0 || borrow.Accounts[12].Address != mapleObligationDebtFarm || borrow.Accounts[13].Address != mapleDebtFarm {
		t.Fatalf("borrow packet did not pin live farm accounts: %+v", borrow)
	}
	repay, err := manifest.kaminoPacketForRoute(DeleverRouteStep, kaminoLegRepay, 77, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 9}, SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	if repay.Policy != mustBasicPolicy(t, BasicDebtLifecycle).Policy || repay.PolicyConstraintIndex != 1 || repay.Accounts[9].Address != mapleObligationDebtFarm || repay.Accounts[10].Address != mapleDebtFarm {
		t.Fatalf("repay packet did not pin live farm accounts: %+v", repay)
	}
}

func TestPhase2MapleSignedKaminoWirePassesPersistenceValidation(t *testing.T) {
	manifest := basicPolicyFixtureManifest(t)
	request, err := manifest.kaminoPacketForRoute(OpenRouteStep, kaminoLegDeposit, 1_000_000, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	signed, err := buildAndSignKaminoPrimeUSDCTransactionForDelegate(request, key, delegate)
	if err != nil {
		t.Fatal(err)
	}
	result, err := signed.BuildResult(123)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.validateForDelegate(delegate); err != nil {
		t.Fatalf("exact Maple Kamino wire was rejected before persistence: %v", err)
	}
}

func TestPhase2BasicFamilyBindingsCoverAllRuntimeLanes(t *testing.T) {
	manifest := basicPolicyFixtureManifest(t)
	for _, lane := range []string{PhaseOneLaneID, SelectedRouteID, "OnRe/ONyc/USDC"} {
		route, err := runtimeRoute(lane)
		if err != nil || !route.BasicPolicy {
			t.Fatalf("%s is not a basic runtime route: %+v, %v", lane, route, err)
		}
		if _, err := fixedRouteAction(OpenRouteStep, lane); err != nil {
			t.Fatalf("%s lifecycle action was not dispatchable: %v", lane, err)
		}
		for _, test := range []struct {
			leg    kaminoPrimeUSDCLeg
			action Action
			family BasicPolicyFamily
			index  byte
		}{
			{kaminoLegDeposit, OpenRouteStep, BasicCollateralLifecycle, 0},
			{kaminoLegWithdraw, DeleverRouteStep, BasicCollateralLifecycle, 1},
			{kaminoLegBorrow, OpenRouteStep, BasicDebtLifecycle, 0},
			{kaminoLegRepay, DeleverRouteStep, BasicDebtLifecycle, 1},
		} {
			request, err := manifest.kaminoPacketForRoute(test.action, test.leg, 77, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 9}, lane)
			if err != nil {
				t.Fatalf("%s %v: %v", lane, test.leg, err)
			}
			policy, _ := basicPolicyBinding(test.family)
			if request.Policy != policy.Policy || request.PolicyConstraintIndex != test.index {
				t.Fatalf("%s %v resolved to policy=%s index=%d", lane, test.leg, request.Policy, request.PolicyConstraintIndex)
			}
			if _, got, err := kaminoRouteInstruction(request, lane); err != nil || got != test.leg {
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
			binding, err := manifest.jupiterPolicyForRoute(test.action, lane)
			if err != nil {
				t.Fatalf("%s %s: %v", lane, test.action, err)
			}
			policy, _ := basicPolicyBinding(test.family)
			wantIndex := byte(0)
			if lane == SelectedRouteID {
				wantIndex = 1
			}
			if binding.Policy != policy.Policy || binding.PolicyConstraintIndex != wantIndex {
				t.Fatalf("%s %s resolved to policy=%s index=%d", lane, test.action, binding.Policy, binding.PolicyConstraintIndex)
			}
		}
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
	manifest := basicPolicyFixtureManifest(t)
	var err error
	entry, err := manifest.jupiterPolicyForRoute(SwapStableToCollateralStep, SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	exit, err := manifest.jupiterPolicyForRoute(SwapCollateralToStableStep, SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Policy != mustBasicPolicy(t, BasicSwapRoutesA).Policy || entry.PolicyConstraintIndex != 1 ||
		exit.Policy != mustBasicPolicy(t, BasicSwapRoutesB).Policy || exit.PolicyConstraintIndex != 1 {
		t.Fatalf("unexpected Phase 2 basic swap bindings: entry=%+v exit=%+v", entry, exit)
	}
}

func TestPhase2JupiterQuotePinsManifestVenue(t *testing.T) {
	quote := JupiterQuote{
		InputMint: bridgeUSDC, OutputMint: mapleSyrupUSDCUSDC.Kamino.CollateralMint,
		InAmount: "1000000", OutAmount: "846514", OtherAmountThreshold: "842281",
		SwapMode: "ExactIn", SlippageBPS: 50, PlatformFee: json.RawMessage("null"),
		RoutePlan: []json.RawMessage{json.RawMessage(`{"swapInfo":{"label":"Manifest"}}`)},
	}
	if _, _, err := validateJupiterQuoteForRoute(quote, SwapUSDCToPrimeStep, 1_000_000, SelectedRouteID); err != nil {
		t.Fatalf("basic quote rejected: %v", err)
	}
	quote.RoutePlan = []json.RawMessage{json.RawMessage(`{"swapInfo":{"label":"AlphaQ"}}`)}
	if _, _, err := validateJupiterQuoteForRoute(quote, SwapUSDCToPrimeStep, 1_000_000, SelectedRouteID); err != nil {
		t.Fatalf("basic quote with recorded route plan rejected: %v", err)
	}
}

func mustBasicPolicy(t *testing.T, family BasicPolicyFamily) BasicPolicyBinding {
	t.Helper()
	binding, err := basicPolicyBinding(family)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func TestPhase2AllocationLeavesRoomForCanaryFeesAndLeverage(t *testing.T) {
	decision := Decide(Snapshot{
		ObservationID: "cap", Slot: 1, RouteKind: RouteKind, RouteLane: SelectedRouteID,
		Fresh: true, VoltrIdleRaw: Phase2TransactionCapRaw + 1,
		PolicyReady: true, ExitBuildable: true, CapacityRaw: Phase2TransactionCapRaw + 1, PolicyLimitRaw: Phase2TransactionCapRaw + 1, MaxTargetLTVEntryRaw: Phase2TransactionCapRaw + 1,
	})
	if decision.Action != VoltrAllocateToSquads || decision.AmountRaw != 500_000 {
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

func TestPreparedDecisionEqualityUsesRefreshedIdentityButPinsSemantics(t *testing.T) {
	prepared := Decision{Action: OpenRouteStep, Reason: "prime_collateral_ready", AmountRaw: 1_000_000, IdempotencyKey: "old-observation", StrategyKey: SelectedRouteID}
	refreshed := prepared
	refreshed.IdempotencyKey = "new-accrued-observation"
	if !decisionsEqual(prepared, refreshed) {
		t.Fatal("unchanged selected-lane execution semantics were rejected after a refreshed observation")
	}
	refreshed.StrategyKey = RouteID
	if decisionsEqual(prepared, refreshed) {
		t.Fatal("decision equality accepted a different runtime lane")
	}
	refreshed = prepared
	refreshed.AmountRaw--
	if decisionsEqual(prepared, refreshed) {
		t.Fatal("decision equality accepted a changed amount")
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
	leg, receiptRaw, collateralRaw, err := selectKaminoLeg(false, decision, KaminoPosition{
		HasPosition: true, CollateralDepositedRaw: 1_500_000, RedeemablePrimeRaw: 1_200_000,
	})
	if err != nil || leg != kaminoLegWithdraw || receiptRaw != 1_000_000 || collateralRaw != 800_000 {
		t.Fatalf("partial legacy withdrawal is wrong: leg=%v receipt=%d collateral=%d err=%v", leg, receiptRaw, collateralRaw, err)
	}
}

func TestPhase2CutoverFundsMaxLTVRepaymentBeforeCollateralRelease(t *testing.T) {
	decision := Decide(Snapshot{
		ObservationID: "cutover-repayment", Slot: 1, RouteKind: RouteKind, RouteLane: RouteID,
		Fresh: true, CutoverDrain: true, HasPosition: true,
		VoltrIdleRaw:            1_000_000,
		PositionCollateralRaw:   1_695_770,
		PositionDebtRaw:         896_575,
		StrategyNAVRaw:          1_793_206,
		LTVBPS:                  4_999,
		LiquidationThresholdBPS: 9_000,
	})
	if decision.Action != VoltrAllocateToSquads || decision.Reason != "phase2_cutover_fund_repayment" || decision.AmountRaw != 896_575 || decision.StrategyKey != RouteID {
		t.Fatalf("max-LTV cutover did not fund repayment first: %+v", decision)
	}
}
