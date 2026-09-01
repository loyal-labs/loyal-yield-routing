package backyardrwa

import (
	"crypto/sha256"
	_ "embed"
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
		m.Identities.SquadsProgram != bridgeSquadsProgram || m.Identities.SquadsSettings != bridgeSettings ||
		m.Identities.SquadsVaultIndex != 0 || m.Identities.SquadsVault != bridgeVault ||
		m.Identities.DelegatedExecutor != bridgeDelegate || m.Identities.SquadsUSDCAta != bridgeSquadsATA ||
		m.Identities.USDCMint != bridgeUSDC || m.Identities.ClassicToken != classicTokenProgram ||
		m.Identities.Token2022 != token2022Program {
		return fmt.Errorf("embedded Backyard manifest does not match pinned bridge identities")
	}
	return nil
}

func (m RouteManifest) executionBlocker() *RuntimeBlocker {
	if m.Status != "ready" || len(m.Unresolved) != 0 || m.PolicyCatalog.Schema != "loyal-backyard-rwa-policy-catalog/v1" ||
		m.PolicyCatalog.SHA256 == nil || !sha256Pattern.MatchString(*m.PolicyCatalog.SHA256) ||
		!m.PolicyCatalog.AddressesResolved || m.PolicyCatalog.PackingRung == nil || *m.PolicyCatalog.PackingRung <= 0 ||
		len(m.PolicyCatalog.PolicyAccounts) == 0 || m.Deployment.SourceCommit == nil ||
		m.Deployment.ImageDigest == nil || m.Deployment.SingleWriterService == nil {
		return ErrBridgePrerequisitesUnavailable
	}
	return nil
}
