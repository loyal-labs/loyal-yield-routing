package backyardrwa

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// embeddedBackyardManifest is a generated, byte-for-byte runtime counterpart
// of docs/manifests/backyard-rwa-v1.json. Keeping it next to the binary makes
// the deployment consume a reviewed manifest rather than deployment variables.
//
//go:embed manifest/backyard-rwa-v1.json
var embeddedBackyardManifest []byte

type RouteManifest struct {
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
	} `json:"policyCatalog"`
	RuntimeBindings struct {
		BridgePolicies []struct {
			Action     Action  `json:"action"`
			Account    string  `json:"account"`
			DataSHA256 *string `json:"dataSha256"`
		} `json:"bridgePolicies"`
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
	SelectedLane        string              `json:"selectedLane"`
	RuntimeRoutes       []string            `json:"runtimeRoutes"`
	SelectedLaneBinding SelectedLaneBinding `json:"selectedLaneBinding"`
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
	if lane == SelectedRouteID {
		mapped := action
		if action == SwapStableToCollateralStep {
			mapped = SwapUSDCToPrimeStep
		} else if action == SwapCollateralToStableStep {
			mapped = SwapPrimeToUSDCStep
		}
		if mapped != SwapUSDCToPrimeStep && mapped != SwapPrimeToUSDCStep {
			return JupiterPolicyBinding{}, fmt.Errorf("action %s is not a Maple swap", action)
		}
		neutral := action
		if action == SwapUSDCToPrimeStep {
			neutral = SwapStableToCollateralStep
		} else if action == SwapPrimeToUSDCStep {
			neutral = SwapCollateralToStableStep
		}
		policy := mapleSyrupUSDCUSDC.PolicyAccounts[neutral]
		hash := mapleSyrupUSDCUSDC.PolicyHashes[neutral]
		if policy == "" || !validSHA256(hash) {
			return JupiterPolicyBinding{}, ErrBridgePrerequisitesUnavailable
		}
		prefix := "01010000007400640001"
		if neutral == SwapCollateralToStableStep {
			prefix = "02010000007400640001"
		}
		return JupiterPolicyBinding{Action: mapped, Policy: policy, PolicyAccountDataSHA256: hash, PolicyConstraintIndex: 0, InstructionDataLength: 37, AmountOffset: 18, ConstraintBindings: []JupiterConstraintBinding{{RoutePlanPrefixHex: prefix, PolicyConstraintIndex: 0}}}, nil
	}
	return m.jupiterPolicy(action)
}

func (b JupiterPolicyBinding) constraintIndex(instruction JupiterSwapInstruction) (byte, error) {
	data, err := base64.StdEncoding.Strict().DecodeString(instruction.Data)
	if err != nil || len(data) != b.InstructionDataLength || b.AmountOffset != len(data)-19 {
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
	hash := sha256.Sum256(embeddedBackyardManifest)
	manifest.SHA256 = hex.EncodeToString(hash[:])
	if err := manifest.validateBindings(); err != nil {
		return RouteManifest{}, err
	}
	return manifest, nil
}

func (m RouteManifest) validateBindings() error {
	if m.Schema != "loyal-backyard-rwa-manifest/v1" || m.Cluster != "mainnet-beta" ||
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
	if m.RuntimeActivation.SelectedLane != "" {
		if m.RuntimeActivation.SelectedLane != SelectedRouteID || len(m.RuntimeActivation.RuntimeRoutes) != RuntimeRouteCount ||
			m.RuntimeActivation.RuntimeRoutes[0] != PhaseOneLaneID || m.RuntimeActivation.RuntimeRoutes[1] != SelectedRouteID {
			return fmt.Errorf("embedded Backyard manifest has an invalid Phase 2 runtime allowlist")
		}
		if err := m.validateSelectedLaneBinding(); err != nil {
			return err
		}
	}
	if m.RuntimeBindings.PrimeUSDC.Program != kaminoProgram ||
		m.RuntimeBindings.PrimeUSDC.Market != kaminoMarket ||
		m.RuntimeBindings.PrimeUSDC.Obligation != kaminoPrimeUSDCObligation ||
		m.RuntimeBindings.PrimeUSDC.CollateralReserve != kaminoCollateralReserve ||
		m.RuntimeBindings.PrimeUSDC.DebtReserve != kaminoDebtReserve ||
		m.RuntimeBindings.PrimeUSDC.CollateralMint != kaminoPrimeMint ||
		m.RuntimeBindings.PrimeUSDC.DebtMint != kaminoUSDCMint {
		return fmt.Errorf("embedded Backyard manifest does not match pinned PRIME/USDC identities")
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
		seen[binding.Action] = true
	}
	if len(m.RuntimeBindings.PrimeUSDC.SwapPolicies) != 2 {
		return fmt.Errorf("embedded Backyard manifest has an incomplete swap policy set")
	}
	for _, expected := range []struct {
		action Action
		index  byte
	}{{SwapUSDCToPrimeStep, 0}, {SwapPrimeToUSDCStep, 1}} {
		binding, err := m.jupiterPolicy(expected.action)
		if err != nil || binding.PolicyConstraintIndex != expected.index {
			return fmt.Errorf("embedded Backyard manifest has a drifted swap policy binding")
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

func (m RouteManifest) bridgePolicy(action Action) (string, string, error) {
	for _, binding := range m.RuntimeBindings.BridgePolicies {
		if binding.Action != action {
			continue
		}
		if binding.DataSHA256 == nil || !sha256Pattern.MatchString(*binding.DataSHA256) {
			return "", "", ErrBridgePrerequisitesUnavailable
		}
		return binding.Account, *binding.DataSHA256, nil
	}
	return "", "", fmt.Errorf("action %s has no fixed bridge policy", action)
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
	if lane != SelectedRouteID {
		request, err := m.primeUSDCPacket(action, leg, amount, blockhash)
		if err == nil {
			request.RouteLane = lane
		}
		return request, err
	}
	if amount == 0 || blockhash.Blockhash == "" || blockhash.LastValidBlockHeight <= 0 {
		return KaminoPrimeUSDCRequest{}, ErrBridgePrerequisitesUnavailable
	}
	policies := map[kaminoPrimeUSDCLeg]struct{ policy, hash string }{
		kaminoLegDeposit:  {"5NyDUfvT3a5gKgh6KMn7qYi5Tp9YfCDUjiJYV1TsnX5c", "501365503468a54060e602ab7fcbe9671c25b817dd5693c1e17c9a6ad90e679f"},
		kaminoLegBorrow:   {"DTVaAQuhRGLrgbopmutf8ePhQbZgpouYDp2YfW8GBUWf", "ec28bbdd6239985c3675e730262ce6c8ddab2ac3c47f54a2e1bb651b851f116f"},
		kaminoLegRepay:    {"CNjRx4kgrAvk6nRGN8NbH8uvSTzoC9zBK1vuiAh6DcYZ", "da76b5a0e4ca7bbc3e69a978ed34467623854e9c99d7ce6b8b844a80db675e09"},
		kaminoLegWithdraw: {"4ZRoNsVZCNJXUdNjFL6MvjMhbLFG512hjStfipMftzcY", "e994455d6351a4f615ae57dd0b0b65287e8c6af10457e70383307bb43c762a7e"},
	}
	metaSets := func() []KaminoPrimeUSDCAccounts {
		deposit, borrow, repay, withdraw := mapleKaminoMetas()
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
	entry, ok := policies[leg]
	if !ok {
		return KaminoPrimeUSDCRequest{}, ErrBridgePrerequisitesUnavailable
	}
	discriminators := map[kaminoPrimeUSDCLeg][]byte{kaminoLegDeposit: kaminoDepositCollateral, kaminoLegBorrow: kaminoBorrowUSDC, kaminoLegRepay: kaminoRepayUSDC, kaminoLegWithdraw: kaminoWithdrawCollateral}
	data := make([]byte, 16)
	copy(data, discriminators[leg])
	for i := 0; i < 8; i++ {
		data[8+i] = byte(amount >> (8 * i))
	}
	request := KaminoPrimeUSDCRequest{Action: action, AmountRaw: amount, Policy: entry.policy, PolicyAccountDataSHA256: entry.hash, PolicyConstraintIndex: 0, Accounts: sets[index], Data: data, RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight, RouteLane: lane}
	if _, observedLeg, err := kaminoRouteInstruction(request, lane); err != nil || observedLeg != leg {
		return KaminoPrimeUSDCRequest{}, ErrBridgePrerequisitesUnavailable
	}
	return request, nil
}

func (m RouteManifest) executionBlocker() *RuntimeBlocker {
	if m.Status != "ready" || m.hasPhaseOneUnresolved() || m.PolicyCatalog.Schema != "loyal-backyard-rwa-policy-catalog/v1" ||
		m.PolicyCatalog.SHA256 == nil || !sha256Pattern.MatchString(*m.PolicyCatalog.SHA256) ||
		len(m.RuntimeBindings.PrimeUSDC.Packets) < 4 || len(m.RuntimeBindings.PrimeUSDC.SwapPolicies) != 2 {
		return ErrBridgePrerequisitesUnavailable
	}
	for _, binding := range m.RuntimeBindings.BridgePolicies {
		if binding.DataSHA256 == nil || !sha256Pattern.MatchString(*binding.DataSHA256) {
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
	lane := PhaseOneLaneID
	if m.RuntimeActivation.SelectedLane != "" {
		lane = m.RuntimeActivation.SelectedLane
	}
	return runtimeRoute(lane)
}
