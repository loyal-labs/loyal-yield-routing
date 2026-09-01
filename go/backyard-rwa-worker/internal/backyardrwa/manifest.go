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
			SwapPolicies []struct {
				Action                  Action `json:"action"`
				Policy                  string `json:"policy"`
				PolicyAccountDataSHA256 string `json:"policyAccountDataSha256"`
				PolicyConstraintIndex   byte   `json:"policyConstraintIndex"`
			} `json:"swapPolicies"`
		} `json:"primeUsdc"`
	} `json:"runtimeBindings"`
	Deployment struct {
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

type JupiterPolicyBinding struct {
	Action                  Action
	Policy                  string
	PolicyAccountDataSHA256 string
	PolicyConstraintIndex   byte
}

func (m RouteManifest) jupiterPolicy(action Action) (JupiterPolicyBinding, error) {
	if action != SwapUSDCToPrimeStep && action != SwapPrimeToUSDCStep {
		return JupiterPolicyBinding{}, fmt.Errorf("action %s is not a fixed Jupiter edge", action)
	}
	for _, binding := range m.RuntimeBindings.PrimeUSDC.SwapPolicies {
		if binding.Action != action {
			continue
		}
		if _, err := decodeKey(binding.Policy); err != nil || !validSHA256(binding.PolicyAccountDataSHA256) {
			return JupiterPolicyBinding{}, ErrBridgePrerequisitesUnavailable
		}
		return JupiterPolicyBinding(binding), nil
	}
	return JupiterPolicyBinding{}, ErrBridgePrerequisitesUnavailable
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
