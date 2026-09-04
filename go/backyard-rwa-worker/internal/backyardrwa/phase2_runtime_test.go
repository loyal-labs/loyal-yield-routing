package backyardrwa

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"testing"
)

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
	if decision.Action != SwapCollateralToStableStep || decision.Reason != "withdrawal_swap_repayment_buffer" || decision.AmountRaw != 250_000 {
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
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
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
	borrow, err := manifest.kaminoPacketForRoute(OpenRouteStep, kaminoLegBorrow, 77, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 9}, SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	if borrow.Policy != "2m7DpWN1d7UC8iMZyipGzo5SRaBz9Buqhw1VJUTMpLSV" || borrow.Accounts[12].Address != mapleObligationDebtFarm || borrow.Accounts[13].Address != mapleDebtFarm {
		t.Fatalf("borrow packet did not pin live farm accounts: %+v", borrow)
	}
	repay, err := manifest.kaminoPacketForRoute(DeleverRouteStep, kaminoLegRepay, 77, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 9}, SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	if repay.Policy != "AjjV5p7BPCxqaf92EsUjx2bavkTuhjHwiBJMvk8Gh8Uo" || repay.Accounts[9].Address != mapleObligationDebtFarm || repay.Accounts[10].Address != mapleDebtFarm {
		t.Fatalf("repay packet did not pin live farm accounts: %+v", repay)
	}
}

func TestPhase2MapleSignedKaminoWirePassesPersistenceValidation(t *testing.T) {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
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

func TestPhase2JupiterQuotePinsManifestVenue(t *testing.T) {
	quote := JupiterQuote{
		InputMint: bridgeUSDC, OutputMint: mapleSyrupUSDCUSDC.Kamino.CollateralMint,
		InAmount: "1000000", OutAmount: "846514", OtherAmountThreshold: "842281",
		SwapMode: "ExactIn", SlippageBPS: 50, PlatformFee: json.RawMessage("null"),
		RoutePlan: []json.RawMessage{json.RawMessage(`{"swapInfo":{"label":"Manifest"}}`)},
	}
	if _, _, err := validateJupiterQuoteForRoute(quote, SwapUSDCToPrimeStep, 1_000_000, SelectedRouteID); err != nil {
		t.Fatalf("Manifest quote rejected: %v", err)
	}
	quote.RoutePlan = []json.RawMessage{json.RawMessage(`{"swapInfo":{"label":"AlphaQ"}}`)}
	if _, _, err := validateJupiterQuoteForRoute(quote, SwapUSDCToPrimeStep, 1_000_000, SelectedRouteID); err == nil {
		t.Fatal("accepted an unreviewed selected-lane venue")
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
	leg, receiptRaw, collateralRaw, err := selectKaminoLeg(decision, KaminoPosition{
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
