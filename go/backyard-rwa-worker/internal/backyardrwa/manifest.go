package backyardrwa

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// embeddedBackyardManifest is a generated, byte-for-byte runtime counterpart
// of docs/manifests/backyard-rwa-v2.json. Keeping it next to the binary makes
// the deployment consume a reviewed manifest rather than deployment variables.
//
//go:embed manifest/backyard-rwa-v2.json
var embeddedBackyardManifest []byte

// The v1 copy remains embedded only as a compatibility fixture for existing
// packet-construction tests. It is never used as the runtime manifest.
//
//go:embed manifest/backyard-rwa-v1.json
var embeddedLegacyBackyardManifest []byte

type RouteManifest struct {
	// Runtime observation scope; does not alter the signed manifest or admit entry.
	selectorObservation   bool
	observationLane       string
	Schema                string `json:"schema"`
	Status                string `json:"status"`
	Cluster               string `json:"cluster"`
	GenesisHash           string `json:"genesisHash"`
	Commitment            string `json:"commitment"`
	MVPRoute              string `json:"mvpRoute"`
	TargetLTVBPS          int64  `json:"targetLtvBps"`
	HardLTVRule           string `json:"hardLtvRule"`
	WithdrawalWaitSeconds int64  `json:"withdrawalWaitSeconds"`
	NAVMaxAgeSeconds      int64  `json:"navMaxAgeSeconds"`
	VaultCapRaw           string `json:"vaultCapRaw"`
	Identities            struct {
		VoltrProgram      string `json:"voltrProgram"`
		VoltrVault        string `json:"voltrVault"`
		AdaptorProgram    string `json:"adaptorProgram"`
		V2StrategyConfig  string `json:"v2StrategyConfig"`
		ReportTicket      string `json:"reportTicket"`
		ReportTicketBump  int64  `json:"reportTicketBump"`
		ReportTicketLen   int64  `json:"reportTicketStateLength"`
		SquadsProgram     string `json:"squadsProgram"`
		SquadsSettings    string `json:"squadsSettings"`
		SquadsVaultIndex  int64  `json:"squadsVaultIndex"`
		SquadsVault       string `json:"squadsVault"`
		DelegatedExecutor string `json:"delegatedExecutor"`
		SquadsUSDCAta     string `json:"squadsUsdcAta"`
		USDCMint          string `json:"usdcMint"`
		ClassicToken      string `json:"classicTokenProgram"`
		Token2022         string `json:"token2022Program"`
	} `json:"identities"`
	PolicyCatalog struct {
		Schema            string   `json:"schema"`
		SHA256            *string  `json:"sha256"`
		AddressesResolved bool     `json:"addressesResolved"`
		PackingRung       *int64   `json:"packingRung"`
		PolicyAccounts    []string `json:"policyAccounts"`
		Policies          []struct {
			Family     BasicPolicyFamily `json:"family"`
			Seed       uint64            `json:"seed"`
			Account    string            `json:"account"`
			DataSHA256 *string           `json:"dataSha256"`
		} `json:"policies"`
	} `json:"policyCatalog"`
	RuntimeBindings struct {
		BridgePolicies []struct {
			Action           Action     `json:"action"`
			Account          string     `json:"account"`
			NormalizedDigest string     `json:"normalizedDigest"`
			MaskedByteRanges [][2]int64 `json:"maskedByteRanges"`
			DataSHA256Raw    string     `json:"dataSha256Raw"`
		} `json:"bridgePolicies"`
		CollateralLifecycle struct {
			Policy            string          `json:"policy"`
			DataSHA256        *string         `json:"dataSha256"`
			ConstraintIndices map[string]byte `json:"constraintIndices"`
		} `json:"collateralLifecycle"`
		DebtLifecycle struct {
			Policy            string          `json:"policy"`
			DataSHA256        *string         `json:"dataSha256"`
			ConstraintIndices map[string]byte `json:"constraintIndices"`
		} `json:"debtLifecycle"`
		SwapRoutesA struct {
			Policy            string          `json:"policy"`
			DataSHA256        *string         `json:"dataSha256"`
			ConstraintIndices map[string]byte `json:"constraintIndices"`
		} `json:"swapRoutesA"`
		SwapRoutesB struct {
			Policy            string          `json:"policy"`
			DataSHA256        *string         `json:"dataSha256"`
			ConstraintIndices map[string]byte `json:"constraintIndices"`
		} `json:"swapRoutesB"`
		PrimeUSDC struct {
			Program           string `json:"program"`
			Market            string `json:"market"`
			Obligation        string `json:"obligation"`
			CollateralReserve string `json:"collateralReserve"`
			DebtReserve       string `json:"debtReserve"`
			CollateralMint    string `json:"collateralMint"`
			DebtMint          string `json:"debtMint"`
			Packets           []struct {
				Action                  Action                  `json:"action"`
				Policy                  string                  `json:"policy"`
				PolicyAccountDataSHA256 string                  `json:"policyAccountDataSha256"`
				PolicyConstraintIndex   byte                    `json:"policyConstraintIndex"`
				Accounts                KaminoPrimeUSDCAccounts `json:"accounts"`
				DataBase64              string                  `json:"dataBase64"`
			} `json:"packets"`
			SwapPolicies []JupiterPolicyBinding `json:"swapPolicies"`
		} `json:"primeUsdc"`
	} `json:"runtimeBindings"`
	// RuntimeActivation is deliberately a two-entry allowlist. The worker
	// reads this as a deployment assertion, never as caller-selected routing.
	RuntimeActivation RuntimeActivation `json:"runtimeActivation"`
	Deployment        struct {
		SourceCommit        *string `json:"sourceCommit"`
		ImageDigest         *string `json:"imageDigest"`
		SingleWriterService *string `json:"singleWriterService"`
	} `json:"deployment"`
	Unresolved []struct {
		Code            string `json:"code"`
		ResumeCondition string `json:"resumeCondition"`
	} `json:"unresolved"`
	SHA256 string `json:"-"`
}

type RuntimeActivation struct {
	SelectedLane        string               `json:"selectedLane"`
	RuntimeRoutes       []RuntimeLaneBinding `json:"runtimeRoutes"`
	SelectedLaneBinding SelectedLaneBinding  `json:"selectedLaneBinding"`
}

type RuntimeLaneBinding struct {
	Lane             string `json:"lane"`
	Protocol         string `json:"protocol"`
	CollateralSymbol string `json:"collateralSymbol"`
	DebtSymbol       string `json:"debtSymbol"`
	Graph            struct {
		KLendProgram              string `json:"klendProgram"`
		Vault                     string `json:"vault"`
		Market                    string `json:"market"`
		MarketAuthority           string `json:"marketAuthority"`
		CollateralReserve         string `json:"collateralReserve"`
		CollateralMint            string `json:"collateralMint"`
		CollateralLiquiditySupply string `json:"collateralLiquiditySupply"`
		CollateralReceiptMint     string `json:"collateralReceiptMint"`
		CollateralReceiptSupply   string `json:"collateralReceiptSupply"`
		CollateralCustody         string `json:"collateralCustody"`
		DebtReserve               string `json:"debtReserve"`
		DebtMint                  string `json:"debtMint"`
		DebtTokenProgram          string `json:"debtTokenProgram"`
		DebtLiquiditySupply       string `json:"debtLiquiditySupply"`
		DebtFeeReceiver           string `json:"debtFeeReceiver"`
		DebtCustody               string `json:"debtCustody"`
		Obligation                string `json:"obligation"`
		CollateralFarmState       string `json:"collateralFarmState"`
		CollateralFarmUserState   string `json:"collateralFarmUserState"`
		DebtFarmState             string `json:"debtFarmState"`
		DebtFarmUserState         string `json:"debtFarmUserState"`
	} `json:"graph"`
}

type SelectedLaneBinding struct {
	Lane  string `json:"lane"`
	Graph struct {
		KLendProgram           string `json:"klendProgram"`
		Vault                  string `json:"vault"`
		LendingMarket          string `json:"lendingMarket"`
		LendingMarketAuthority string `json:"lendingMarketAuthority"`
		Obligation             string `json:"obligation"`
		CollateralReserve      struct {
			Address          string `json:"address"`
			LiquidityMint    string `json:"liquidityMint"`
			LiquiditySupply  string `json:"liquiditySupply"`
			CollateralMint   string `json:"collateralMint"`
			CollateralSupply string `json:"collateralSupply"`
		} `json:"collateralReserve"`
		DebtReserve struct {
			Address              string `json:"address"`
			LiquidityMint        string `json:"liquidityMint"`
			LiquiditySupply      string `json:"liquiditySupply"`
			LiquidityFeeReceiver string `json:"liquidityFeeReceiver"`
		} `json:"debtReserve"`
		CollateralCustody struct {
			Address string `json:"address"`
		} `json:"collateralCustody"`
		DebtCustody struct {
			Address string `json:"address"`
		} `json:"debtCustody"`
	} `json:"graph"`
	KaminoPolicies []struct {
		Operation             string   `json:"operation"`
		Policy                string   `json:"policy"`
		LiveAccountDataSHA256 string   `json:"liveAccountDataSha256"`
		ProgramID             string   `json:"programId"`
		AccountPubkeys        []string `json:"accountPubkeys"`
	} `json:"kaminoPolicies"`
	JupiterEdges []struct {
		Edge                  string `json:"edge"`
		Policy                string `json:"policy"`
		LiveAccountDataSHA256 string `json:"liveAccountDataSha256"`
		ProgramID             string `json:"programId"`
		SourceMint            string `json:"sourceMint"`
		DestinationMint       string `json:"destinationMint"`
		SourceCustody         string `json:"sourceCustody"`
		DestinationCustody    string `json:"destinationCustody"`
	} `json:"jupiterEdges"`
}

type JupiterPolicyBinding struct {
	CatalogLane             string                     `json:"catalogLane,omitempty"`
	BasicPolicy             bool                       `json:"-"`
	Action                  Action                     `json:"action"`
	Policy                  string                     `json:"policy"`
	PolicyAccountDataSHA256 string                     `json:"policyAccountDataSha256"`
	PolicyConstraintIndex   byte                       `json:"policyConstraintIndex"`
	InstructionDataLength   int                        `json:"instructionDataLength"`
	AmountOffset            int                        `json:"amountOffset"`
	ConstraintBindings      []JupiterConstraintBinding `json:"constraintBindings"`
}

type JupiterConstraintBinding struct {
	RoutePlanPrefixHex    string `json:"routePlanPrefixHex"`
	PolicyConstraintIndex byte   `json:"policyConstraintIndex"`
}

func (m RouteManifest) jupiterPolicy(action Action) (JupiterPolicyBinding, error) {
	if action != SwapUSDCToPrimeStep && action != SwapPrimeToUSDCStep {
		return JupiterPolicyBinding{}, fmt.Errorf("action %s is not a fixed Jupiter edge", action)
	}
	for _, binding := range m.RuntimeBindings.PrimeUSDC.SwapPolicies {
		if binding.Action != action {
			continue
		}
		if _, err := decodeKey(binding.Policy); err != nil || !validSHA256(binding.PolicyAccountDataSHA256) ||
			binding.InstructionDataLength != 37 || binding.AmountOffset != 18 {
			return JupiterPolicyBinding{}, ErrBridgePrerequisitesUnavailable
		}
		if action == SwapUSDCToPrimeStep {
			if binding.PolicyConstraintIndex != 0 || len(binding.ConstraintBindings) != 2 ||
				binding.ConstraintBindings[0].RoutePlanPrefixHex != "01010000007400640001" ||
				binding.ConstraintBindings[0].PolicyConstraintIndex != 0 ||
				binding.ConstraintBindings[1].RoutePlanPrefixHex != "02010000007400640001" ||
				binding.ConstraintBindings[1].PolicyConstraintIndex != 1 {
				return JupiterPolicyBinding{}, ErrBridgePrerequisitesUnavailable
			}
		} else if binding.PolicyConstraintIndex != 1 || len(binding.ConstraintBindings) != 0 {
			return JupiterPolicyBinding{}, ErrBridgePrerequisitesUnavailable
		}
		return binding, nil
	}
	return JupiterPolicyBinding{}, ErrBridgePrerequisitesUnavailable
}

func (m RouteManifest) jupiterPolicyForRoute(action Action, lane string) (JupiterPolicyBinding, error) {
	if catalogJupiterRoute(lane) {
		b, err := catalogJupiterBindingForRoute(action, lane)
		if err != nil {
			return JupiterPolicyBinding{}, err
		}
		dataLength := b.FeeOffset + 1
		if b.fixedPrefixV2() {
			dataLength = 0
		} // variable route vector; matchesData validates the fixed prefix
		return JupiterPolicyBinding{CatalogLane: lane, Action: action, Policy: b.Policy, PolicyAccountDataSHA256: b.PolicySHA256,
			PolicyConstraintIndex: b.ConstraintIndex, InstructionDataLength: dataLength, AmountOffset: b.AmountOffset}, nil
	}
	if lane == PhaseOneLaneID || lane == SelectedRouteID || lane == "OnRe/ONyc/USDC" {
		_, _, sourceCustody, destinationCustody, err := jupiterEdgeForRoute(action, lane)
		if err != nil {
			return JupiterPolicyBinding{}, err
		}
		basic, index, err := resolveBasicSwapPolicy(sourceCustody, destinationCustody)
		if err != nil {
			return JupiterPolicyBinding{}, err
		}
		_, hash, err := m.basicPolicyBinding(basic.Family)
		if err != nil {
			return JupiterPolicyBinding{}, err
		}
		return JupiterPolicyBinding{BasicPolicy: true, Action: action, Policy: basic.Policy,
			PolicyAccountDataSHA256: hash, PolicyConstraintIndex: index, AmountOffset: 18}, nil
	}
	if lane != "" && lane != RouteID {
		return JupiterPolicyBinding{}, fmt.Errorf("unregistered Jupiter policy lane")
	}
	return m.jupiterPolicy(action)
}

func (m RouteManifest) basicPolicyBinding(family BasicPolicyFamily) (BasicPolicyBinding, string, error) {
	binding, err := basicPolicyBinding(family)
	if err != nil {
		return BasicPolicyBinding{}, "", err
	}
	var policy string
	var hash *string
	switch family {
	case BasicCollateralLifecycle:
		policy, hash = m.RuntimeBindings.CollateralLifecycle.Policy, m.RuntimeBindings.CollateralLifecycle.DataSHA256
	case BasicDebtLifecycle:
		policy, hash = m.RuntimeBindings.DebtLifecycle.Policy, m.RuntimeBindings.DebtLifecycle.DataSHA256
	case BasicSwapRoutesA:
		policy, hash = m.RuntimeBindings.SwapRoutesA.Policy, m.RuntimeBindings.SwapRoutesA.DataSHA256
	case BasicSwapRoutesB:
		policy, hash = m.RuntimeBindings.SwapRoutesB.Policy, m.RuntimeBindings.SwapRoutesB.DataSHA256
	default:
		return BasicPolicyBinding{}, "", fmt.Errorf("unknown basic policy family %q", family)
	}
	if policy != binding.Policy || hash == nil || !validSHA256(*hash) {
		return BasicPolicyBinding{}, "", ErrBridgePrerequisitesUnavailable
	}
	return binding, *hash, nil
}

func (b JupiterPolicyBinding) constraintIndex(instruction JupiterSwapInstruction) (byte, error) {
	data, err := base64.StdEncoding.Strict().DecodeString(instruction.Data)
	if err != nil {
		return 0, fmt.Errorf("fresh Jupiter header does not match the manifest binding")
	}
	if b.CatalogLane != "" {
		bound, err := catalogJupiterBindingForRoute(b.Action, b.CatalogLane)
		if err != nil || b.Policy != bound.Policy || b.PolicyAccountDataSHA256 != bound.PolicySHA256 || b.PolicyConstraintIndex != bound.ConstraintIndex || b.AmountOffset != bound.AmountOffset || !bound.matchesData(data) {
			return 0, fmt.Errorf("Jupiter catalog policy changed")
		}
		return bound.ConstraintIndex, nil
	}
	if b.BasicPolicy {
		if len(data) < 28 || (!bytes.Equal(data[:8], jupiterSharedAccountsRoute) && !bytes.Equal(data[:8], jupiterSharedAccountsRouteV2)) {
			return 0, fmt.Errorf("fresh Jupiter header does not match the basic policy binding")
		}
		return b.PolicyConstraintIndex, nil
	}
	if len(data) != b.InstructionDataLength || b.AmountOffset != len(data)-19 {
		return 0, fmt.Errorf("fresh Jupiter header does not match the manifest binding")
	}
	if b.Action == SwapPrimeToUSDCStep {
		return b.PolicyConstraintIndex, nil
	}
	if b.Action != SwapUSDCToPrimeStep || len(data) < 18 {
		return 0, fmt.Errorf("fresh Jupiter action does not match the manifest binding")
	}
	prefix := hex.EncodeToString(data[8:18])
	for _, candidate := range b.ConstraintBindings {
		if candidate.RoutePlanPrefixHex == prefix {
			return candidate.PolicyConstraintIndex, nil
		}
	}
	return 0, fmt.Errorf("fresh Jupiter route-plan prefix is not installed")
}

func loadEmbeddedRouteManifest() (RouteManifest, error) {
	var manifest RouteManifest
	if err := json.Unmarshal(embeddedBackyardManifest, &manifest); err != nil {
		return RouteManifest{}, fmt.Errorf("decode embedded Backyard manifest: %w", err)
	}
	// Keep the old packet-shaped field available to existing construction
	// fixtures while the v2 runtime source of truth is family-shaped. Phase 2
	// replaces these compatibility reads with the generic family resolver.
	if len(manifest.RuntimeBindings.PrimeUSDC.Packets) == 0 {
		var legacy struct {
			RuntimeBindings json.RawMessage `json:"runtimeBindings"`
		}
		if err := json.Unmarshal(embeddedLegacyBackyardManifest, &legacy); err != nil {
			return RouteManifest{}, fmt.Errorf("decode embedded legacy Backyard manifest: %w", err)
		}
		var bindings struct {
			PrimeUSDC json.RawMessage `json:"primeUsdc"`
		}
		if err := json.Unmarshal(legacy.RuntimeBindings, &bindings); err != nil || json.Unmarshal(bindings.PrimeUSDC, &manifest.RuntimeBindings.PrimeUSDC) != nil {
			return RouteManifest{}, fmt.Errorf("decode embedded legacy Backyard packet bindings")
		}
	}
	hash := sha256.Sum256(embeddedBackyardManifest)
	manifest.SHA256 = hex.EncodeToString(hash[:])
	if err := manifest.validateBindings(); err != nil {
		return RouteManifest{}, err
	}
	return manifest, nil
}

func (m RouteManifest) validateBindings() error {
	if m.Schema != "loyal-backyard-rwa-manifest/v2" || m.Cluster != "mainnet-beta" ||
		m.Commitment != "confirmed" || m.MVPRoute != RouteID || m.TargetLTVBPS != TargetLTVBPS ||
		m.HardLTVRule != "min(6000, liquidationThresholdBps - 1500)" ||
		m.WithdrawalWaitSeconds != 600 || m.NAVMaxAgeSeconds != 60 || m.VaultCapRaw != "1000000000000" {
		return fmt.Errorf("embedded Backyard manifest has an invalid fixed route")
	}
	if m.Identities.VoltrProgram != bridgeVoltrProgram || m.Identities.VoltrVault != bridgeVoltrVault ||
		m.Identities.AdaptorProgram != bridgeAdaptorProgram || m.Identities.V2StrategyConfig != bridgeStrategy ||
		m.Identities.ReportTicket != reportTicketPDA || m.Identities.ReportTicketBump != int64(reportTicketBump) ||
		m.Identities.ReportTicketLen != reportTicketStateLength ||
		m.Identities.SquadsProgram != bridgeSquadsProgram || m.Identities.SquadsSettings != bridgeSettings ||
		m.Identities.SquadsVaultIndex != 0 || m.Identities.SquadsVault != bridgeVault ||
		m.Identities.DelegatedExecutor != bridgeDelegate || m.Identities.SquadsUSDCAta != bridgeSquadsATA ||
		m.Identities.USDCMint != bridgeUSDC || m.Identities.ClassicToken != classicTokenProgram ||
		m.Identities.Token2022 != token2022Program {
		return fmt.Errorf("embedded Backyard manifest does not match pinned bridge identities")
	}
	if m.RuntimeActivation.SelectedLane != SelectedRouteID || len(m.RuntimeActivation.RuntimeRoutes) != RuntimeRouteCount {
		return fmt.Errorf("embedded Backyard manifest has an invalid basic runtime allowlist")
	}
	for i, expected := range []string{PhaseOneLaneID, SelectedRouteID, "OnRe/ONyc/USDC"} {
		if m.RuntimeActivation.RuntimeRoutes[i].Lane != expected || !validRuntimeLaneBinding(m.RuntimeActivation.RuntimeRoutes[i]) {
			return fmt.Errorf("embedded Backyard manifest has an invalid runtime graph for %q", expected)
		}
	}
	if err := m.validateBasicPolicyBindings(); err != nil {
		return err
	}
	expectedBridgePolicies := map[Action]string{
		VoltrAllocateToSquads: bridgeAllocationPolicy,
		ReportNAV:             bridgeNAVPolicy,
		StageSquadsToVoltr:    bridgeStagePolicy,
		VoltrRestoreIdle:      bridgeWithdrawPolicy,
	}
	if len(m.RuntimeBindings.BridgePolicies) != len(expectedBridgePolicies) {
		return fmt.Errorf("embedded Backyard manifest has an incomplete bridge policy set")
	}
	seen := map[Action]bool{}
	for _, binding := range m.RuntimeBindings.BridgePolicies {
		if expectedBridgePolicies[binding.Action] != binding.Account || seen[binding.Action] {
			return fmt.Errorf("embedded Backyard manifest has a drifted bridge policy identity")
		}
		if !sha256Pattern.MatchString(binding.NormalizedDigest) || !sha256Pattern.MatchString(binding.DataSHA256Raw) {
			return fmt.Errorf("embedded Backyard manifest has a malformed bridge policy digest for %s", binding.Action)
		}
		if err := validatePolicyByteMask(binding.MaskedByteRanges); err != nil {
			return fmt.Errorf("embedded Backyard manifest has an invalid bridge policy mask for %s: %w", binding.Action, err)
		}
		seen[binding.Action] = true
	}
	return nil
}

func validRuntimeLaneBinding(route RuntimeLaneBinding) bool {
	if route.Lane == "" || route.Protocol == "" || route.CollateralSymbol == "" || route.DebtSymbol == "" {
		return false
	}
	graph := route.Graph
	values := []string{
		graph.KLendProgram, graph.Vault, graph.Market, graph.MarketAuthority,
		graph.CollateralReserve, graph.CollateralMint, graph.CollateralLiquiditySupply,
		graph.CollateralReceiptMint, graph.CollateralReceiptSupply, graph.CollateralCustody,
		graph.DebtReserve, graph.DebtMint, graph.DebtTokenProgram, graph.DebtLiquiditySupply,
		graph.DebtFeeReceiver, graph.DebtCustody, graph.Obligation,
	}
	for _, value := range values {
		if value == "" {
			return false
		}
	}
	if (graph.CollateralFarmState == "") != (graph.CollateralFarmUserState == "") ||
		(graph.DebtFarmState == "") != (graph.DebtFarmUserState == "") {
		return false
	}
	return true
}

func (m RouteManifest) validateBasicPolicyBindings() error {
	set, err := basicPolicySet()
	if err != nil {
		return err
	}
	if m.PolicyCatalog.Schema != "loyal-backyard-rwa-policy-catalog/v2" || len(m.PolicyCatalog.Policies) != 4 {
		return fmt.Errorf("embedded Backyard manifest has an incomplete basic policy catalog")
	}
	seen := map[BasicPolicyFamily]bool{}
	for _, entry := range m.PolicyCatalog.Policies {
		expected, ok := set[entry.Family]
		if !ok || seen[entry.Family] || entry.Seed != expected.Seed || entry.Account != expected.Policy {
			return fmt.Errorf("embedded Backyard manifest has a drifted policy catalog entry")
		}
		if entry.DataSHA256 != nil && !validSHA256(*entry.DataSHA256) {
			return fmt.Errorf("embedded Backyard manifest has an invalid policy data hash")
		}
		seen[entry.Family] = true
	}
	for _, family := range []BasicPolicyFamily{BasicCollateralLifecycle, BasicDebtLifecycle, BasicSwapRoutesA, BasicSwapRoutesB} {
		if !seen[family] {
			return fmt.Errorf("embedded Backyard manifest is missing %s", family)
		}
	}
	for _, binding := range []struct {
		family BasicPolicyFamily
		policy string
		data   *string
		want   map[string]byte
		got    map[string]byte
	}{
		{BasicCollateralLifecycle, m.RuntimeBindings.CollateralLifecycle.Policy, m.RuntimeBindings.CollateralLifecycle.DataSHA256, map[string]byte{"deposit": 0, "withdraw": 1}, m.RuntimeBindings.CollateralLifecycle.ConstraintIndices},
		{BasicDebtLifecycle, m.RuntimeBindings.DebtLifecycle.Policy, m.RuntimeBindings.DebtLifecycle.DataSHA256, map[string]byte{"borrow": 0, "repay": 1}, m.RuntimeBindings.DebtLifecycle.ConstraintIndices},
		{BasicSwapRoutesA, m.RuntimeBindings.SwapRoutesA.Policy, m.RuntimeBindings.SwapRoutesA.DataSHA256, map[string]byte{"route0": 0, "route1": 1}, m.RuntimeBindings.SwapRoutesA.ConstraintIndices},
		{BasicSwapRoutesB, m.RuntimeBindings.SwapRoutesB.Policy, m.RuntimeBindings.SwapRoutesB.DataSHA256, map[string]byte{"route0": 0, "route1": 1}, m.RuntimeBindings.SwapRoutesB.ConstraintIndices},
	} {
		expected := set[binding.family]
		if binding.policy != expected.Policy || len(binding.got) != len(binding.want) || len(binding.want) != len(expected.Index) {
			return fmt.Errorf("embedded Backyard manifest has a drifted %s binding", binding.family)
		}
		for key, index := range binding.want {
			if binding.got[key] != index {
				return fmt.Errorf("embedded Backyard manifest has a drifted %s index", binding.family)
			}
		}
		if binding.data != nil && !validSHA256(*binding.data) {
			return fmt.Errorf("embedded Backyard manifest has an invalid %s data hash", binding.family)
		}
	}
	return nil
}

func (m RouteManifest) validateSelectedLaneBinding() error {
	b := m.RuntimeActivation.SelectedLaneBinding
	r := mapleSyrupUSDCUSDC
	if b.Lane != r.Lane || b.Graph.KLendProgram != r.Kamino.Program || b.Graph.Vault != r.Kamino.Vault ||
		b.Graph.LendingMarket != r.Kamino.Market || b.Graph.LendingMarketAuthority != r.Kamino.MarketAuthority ||
		b.Graph.Obligation != r.Kamino.Obligation || b.Graph.CollateralReserve.Address != r.Kamino.CollateralReserve ||
		b.Graph.CollateralReserve.LiquidityMint != r.Kamino.CollateralMint || b.Graph.CollateralReserve.LiquiditySupply != r.CollateralLiquiditySupply ||
		b.Graph.CollateralReserve.CollateralMint != r.CollateralReceiptMint || b.Graph.CollateralReserve.CollateralSupply != r.CollateralReceiptSupply ||
		b.Graph.DebtReserve.Address != r.Kamino.DebtReserve || b.Graph.DebtReserve.LiquidityMint != r.Kamino.DebtMint ||
		b.Graph.DebtReserve.LiquiditySupply != r.DebtLiquiditySupply || b.Graph.DebtReserve.LiquidityFeeReceiver != r.DebtFeeReceiver ||
		b.Graph.CollateralCustody.Address != r.CollateralCustody || b.Graph.DebtCustody.Address != r.DebtCustody {
		return fmt.Errorf("embedded Phase 2 selected-lane graph differs from the compiled runtime graph")
	}
	if len(b.KaminoPolicies) != 4 || len(b.JupiterEdges) != 2 {
		return fmt.Errorf("embedded Phase 2 selected-lane policy binding is incomplete")
	}
	expectedPolicies := mapleKaminoPolicyHashes()
	deposit, borrow, repay, withdraw := mapleKaminoMetas()
	metaAddresses := func(items []accountMeta) []string {
		addresses := make([]string, len(items))
		for i, item := range items {
			addresses[i] = encodeBase58(item.key[:])
		}
		return addresses
	}
	expectedAccounts := map[string][]string{
		"deposit": metaAddresses(deposit), "borrow": metaAddresses(borrow),
		"repay": metaAddresses(repay), "withdraw": metaAddresses(withdraw),
	}
	seen := map[string]bool{}
	for _, policy := range b.KaminoPolicies {
		if policy.ProgramID != r.Kamino.Program || expectedPolicies[policy.Policy] != policy.LiveAccountDataSHA256 || seen[policy.Operation] {
			return fmt.Errorf("embedded Phase 2 Kamino policy binding drifted")
		}
		expected := expectedAccounts[policy.Operation]
		if len(expected) != len(policy.AccountPubkeys) {
			return fmt.Errorf("embedded Phase 2 Kamino %s account graph drifted", policy.Operation)
		}
		for i := range expected {
			if expected[i] != policy.AccountPubkeys[i] {
				return fmt.Errorf("embedded Phase 2 Kamino %s account %d drifted", policy.Operation, i)
			}
		}
		seen[policy.Operation] = true
	}
	for _, operation := range []string{"deposit", "borrow", "repay", "withdraw"} {
		if !seen[operation] {
			return fmt.Errorf("embedded Phase 2 Kamino %s policy is absent", operation)
		}
	}
	expectedEdges := map[string]struct{ policy, hash, sourceMint, destinationMint, sourceCustody, destinationCustody string }{
		"USDC->syrupUSDC": {r.PolicyAccounts[SwapStableToCollateralStep], r.PolicyHashes[SwapStableToCollateralStep], r.Kamino.DebtMint, r.Kamino.CollateralMint, r.DebtCustody, r.CollateralCustody},
		"syrupUSDC->USDC": {r.PolicyAccounts[SwapCollateralToStableStep], r.PolicyHashes[SwapCollateralToStableStep], r.Kamino.CollateralMint, r.Kamino.DebtMint, r.CollateralCustody, r.DebtCustody},
	}
	for _, edge := range b.JupiterEdges {
		expected, ok := expectedEdges[edge.Edge]
		if !ok || edge.ProgramID != jupiterV6Program || edge.Policy != expected.policy || edge.LiveAccountDataSHA256 != expected.hash ||
			edge.SourceMint != expected.sourceMint || edge.DestinationMint != expected.destinationMint || edge.SourceCustody != expected.sourceCustody || edge.DestinationCustody != expected.destinationCustody {
			return fmt.Errorf("embedded Phase 2 Jupiter edge %q binding drifted: %+v expected %+v", edge.Edge, edge, expected)
		}
		delete(expectedEdges, edge.Edge)
	}
	if len(expectedEdges) != 0 {
		return fmt.Errorf("embedded Phase 2 Jupiter edge binding is incomplete")
	}
	return nil
}

// BridgePolicyPin is the manifest-pinned identity of one bridge policy: the
// account, the masked normalized digest its bytes must hash to, and the byte
// ranges the mask excludes.
type BridgePolicyPin struct {
	Account          string
	NormalizedDigest string
	MaskedByteRanges [][2]int64
}

func (m RouteManifest) bridgePolicy(action Action) (BridgePolicyPin, error) {
	for _, binding := range m.RuntimeBindings.BridgePolicies {
		if binding.Action != action {
			continue
		}
		if !sha256Pattern.MatchString(binding.NormalizedDigest) {
			return BridgePolicyPin{}, ErrBridgePrerequisitesUnavailable
		}
		return BridgePolicyPin{Account: binding.Account, NormalizedDigest: binding.NormalizedDigest, MaskedByteRanges: binding.MaskedByteRanges}, nil
	}
	return BridgePolicyPin{}, fmt.Errorf("action %s has no fixed bridge policy", action)
}

func (m RouteManifest) primeUSDCPacket(action Action, leg kaminoPrimeUSDCLeg, amount uint64, blockhash LatestBlockhash) (KaminoPrimeUSDCRequest, error) {
	if amount == 0 || blockhash.Blockhash == "" || blockhash.LastValidBlockHeight <= 0 {
		return KaminoPrimeUSDCRequest{}, ErrBridgePrerequisitesUnavailable
	}
	var expected []byte
	switch leg {
	case kaminoLegDeposit:
		expected = kaminoDepositCollateral
	case kaminoLegBorrow:
		expected = kaminoBorrowUSDC
	case kaminoLegRepay:
		expected = kaminoRepayUSDC
	case kaminoLegWithdraw:
		expected = kaminoWithdrawCollateral
	default:
		return KaminoPrimeUSDCRequest{}, fmt.Errorf("unknown PRIME/USDC leg")
	}
	for _, binding := range m.RuntimeBindings.PrimeUSDC.Packets {
		if binding.Action != action || !sha256Pattern.MatchString(binding.PolicyAccountDataSHA256) {
			continue
		}
		data, err := base64.StdEncoding.Strict().DecodeString(binding.DataBase64)
		if err != nil || len(data) != 16 || !bytesEqual(data[:8], expected) || readU64(data[8:]) != 0 {
			continue
		}
		data = append([]byte(nil), data...)
		for index := 0; index < 8; index++ {
			data[8+index] = byte(amount >> (8 * index))
		}
		request := KaminoPrimeUSDCRequest{
			Action: action, AmountRaw: amount, Policy: binding.Policy,
			PolicyConstraintIndex:   binding.PolicyConstraintIndex,
			PolicyAccountDataSHA256: binding.PolicyAccountDataSHA256,
			Accounts:                binding.Accounts, Data: data, RecentBlockhash: blockhash.Blockhash,
			LastValidBlockHeight: blockhash.LastValidBlockHeight,
		}
		if _, observedLeg, err := kaminoPrimeUSDCInstruction(request); err == nil && observedLeg == leg {
			return request, nil
		}
	}
	return KaminoPrimeUSDCRequest{}, ErrBridgePrerequisitesUnavailable
}

func (m RouteManifest) kaminoPacketForRoute(action Action, leg kaminoPrimeUSDCLeg, amount uint64, blockhash LatestBlockhash, lane string) (KaminoPrimeUSDCRequest, error) {
	if lane == "" || lane == RouteID {
		request, err := m.primeUSDCPacket(action, leg, amount, blockhash)
		if err == nil {
			request.RouteLane = lane
		}
		return request, err
	}
	if amount == 0 || blockhash.Blockhash == "" || blockhash.LastValidBlockHeight <= 0 {
		return KaminoPrimeUSDCRequest{}, ErrBridgePrerequisitesUnavailable
	}
	route, err := runtimeRoute(lane)
	if err != nil {
		return KaminoPrimeUSDCRequest{}, ErrBridgePrerequisitesUnavailable
	}
	var policy, policyHash string
	if route.BasicPolicy {
		family := basicPolicyFamilyForKaminoLeg(leg)
		binding, hash, err := m.basicPolicyBinding(family)
		if err != nil {
			return KaminoPrimeUSDCRequest{}, err
		}
		policy, policyHash = binding.Policy, hash
	}
	metaSets := func() []KaminoPrimeUSDCAccounts {
		deposit, borrow, repay, withdraw := kaminoMetasForRoute(route)
		convert := func(input []accountMeta) KaminoPrimeUSDCAccounts {
			out := make(KaminoPrimeUSDCAccounts, len(input))
			for i, item := range input {
				out[i] = struct {
					Address  string
					Signer   bool
					Writable bool
				}{encodeBase58(item.key[:]), item.signer, item.writable}
			}
			return out
		}
		return []KaminoPrimeUSDCAccounts{convert(deposit), convert(borrow), convert(repay), convert(withdraw)}
	}
	sets := metaSets()
	index := int(leg) - 1
	if index < 0 || index >= len(sets) {
		return KaminoPrimeUSDCRequest{}, ErrBridgePrerequisitesUnavailable
	}
	if !route.BasicPolicy {
		entry, ok := route.KaminoPolicies[leg]
		if !ok {
			return KaminoPrimeUSDCRequest{}, ErrBridgePrerequisitesUnavailable
		}
		policy, policyHash = entry.Policy, entry.DataSHA256
	}
	discriminators := map[kaminoPrimeUSDCLeg][]byte{kaminoLegDeposit: kaminoDepositCollateral, kaminoLegBorrow: kaminoBorrowUSDC, kaminoLegRepay: kaminoRepayUSDC, kaminoLegWithdraw: kaminoWithdrawCollateral}
	data := make([]byte, 16)
	copy(data, discriminators[leg])
	for i := 0; i < 8; i++ {
		data[8+i] = byte(amount >> (8 * i))
	}
	request := KaminoPrimeUSDCRequest{Action: action, AmountRaw: amount, Policy: policy, PolicyAccountDataSHA256: policyHash, PolicyConstraintIndex: kaminoConstraintIndexForRoute(route, leg), Accounts: sets[index], Data: data, RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight, RouteLane: lane}
	if _, observedLeg, err := kaminoRouteInstruction(request, lane); err != nil || observedLeg != leg {
		return KaminoPrimeUSDCRequest{}, ErrBridgePrerequisitesUnavailable
	}
	return request, nil
}

func (m RouteManifest) executionBlocker() *RuntimeBlocker {
	if m.Status != "ready" || m.hasPhaseOneUnresolved() || m.PolicyCatalog.Schema != "loyal-backyard-rwa-policy-catalog/v2" ||
		m.PolicyCatalog.SHA256 == nil || !sha256Pattern.MatchString(*m.PolicyCatalog.SHA256) ||
		len(m.PolicyCatalog.Policies) != 4 {
		return ErrBridgePrerequisitesUnavailable
	}
	for _, policy := range m.PolicyCatalog.Policies {
		if policy.DataSHA256 == nil || !sha256Pattern.MatchString(*policy.DataSHA256) {
			return ErrBridgePrerequisitesUnavailable
		}
	}
	for _, binding := range m.RuntimeBindings.BridgePolicies {
		if !sha256Pattern.MatchString(binding.NormalizedDigest) || !sha256Pattern.MatchString(binding.DataSHA256Raw) {
			return ErrBridgePrerequisitesUnavailable
		}
	}
	for _, hash := range []*string{
		m.RuntimeBindings.CollateralLifecycle.DataSHA256,
		m.RuntimeBindings.DebtLifecycle.DataSHA256,
		m.RuntimeBindings.SwapRoutesA.DataSHA256,
		m.RuntimeBindings.SwapRoutesB.DataSHA256,
	} {
		if hash == nil || !sha256Pattern.MatchString(*hash) {
			return ErrBridgePrerequisitesUnavailable
		}
	}
	return nil
}

func (m RouteManifest) hasPhaseOneUnresolved() bool {
	for _, unresolved := range m.Unresolved {
		// The remaining eleven-lane catalog is installed after the bridge and
		// fixed PRIME/USDC lifecycle. It must not disable that first release.
		if unresolved.Code != "UNRESOLVED_CURRENT_POLICY_GRAPH" {
			return true
		}
	}
	return false
}

func (m RouteManifest) activeRuntimeRoute() (RuntimeRoute, error) {
	if m.observationLane != "" {
		if !m.selectorObservation || !selectorLane(m.observationLane) {
			return RuntimeRoute{}, fmt.Errorf("unadmitted observation lane")
		}
		return runtimeRoute(m.observationLane)
	}
	lane := PhaseOneLaneID
	if m.RuntimeActivation.SelectedLane != "" {
		lane = m.RuntimeActivation.SelectedLane
	}
	return runtimeRoute(lane)
}
