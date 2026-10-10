package backyard

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

// embeddedBackyardManifest is the reviewed manifest the deployment consumes,
// next to the binary rather than in deployment variables.
//
//go:embed manifest/backyard-rwa-v2.json
var embeddedBackyardManifest []byte

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
	// RuntimeBindings carries the legacy PRIME/USDC route's packets: each
	// lifecycle leg's exact KLend accounts and instruction data.
	RuntimeBindings struct {
		PrimeUSDC struct {
			Packets []struct {
				Action     Action                  `json:"action"`
				Accounts   KaminoPrimeUSDCAccounts `json:"accounts"`
				DataBase64 string                  `json:"dataBase64"`
			} `json:"packets"`
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
	SelectedLane  string               `json:"selectedLane"`
	RuntimeRoutes []RuntimeLaneBinding `json:"runtimeRoutes"`
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
	if m.Schema != "loyal-backyard-rwa-manifest/v2" || m.Cluster != "mainnet-beta" ||
		m.Commitment != "confirmed" || m.MVPRoute != RouteID || m.TargetLTVBPS != TargetLTVBPS ||
		m.HardLTVRule != "min(6000, liquidationThresholdBps - 1500)" ||
		m.WithdrawalWaitSeconds != 600 || m.NAVMaxAgeSeconds != 60 || m.VaultCapRaw != "1000000000000" {
		return fmt.Errorf("embedded Backyard manifest has an invalid fixed route")
	}
	if m.Identities.VoltrProgram != voltr.ProgramID.String() || m.Identities.VoltrVault != bridgeVoltrVault ||
		m.Identities.AdaptorProgram != bridgeAdaptorProgram || m.Identities.V2StrategyConfig != bridgeStrategy ||
		m.Identities.ReportTicket != reportTicketPDA || m.Identities.ReportTicketBump != int64(reportTicketBump) ||
		m.Identities.ReportTicketLen != reportTicketStateLength ||
		m.Identities.SquadsProgram != squads.ProgramID.String() || m.Identities.SquadsSettings != bridgeSettings ||
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

// primeUSDCPacket is the legacy PRIME/USDC route's packet for leg, for amount,
// executed through the leg's installed policy.
func (m RouteManifest) primeUSDCPacket(policies installedPolicies, action Action, leg kaminoPrimeUSDCLeg, amount uint64, blockhash LatestBlockhash) (KaminoPrimeUSDCRequest, error) {
	if amount == 0 || blockhash.Blockhash == "" || blockhash.LastValidBlockHeight <= 0 {
		return KaminoPrimeUSDCRequest{}, ErrBridgePrerequisitesUnavailable
	}
	discriminator := kaminoLegDiscriminator(leg)
	if discriminator == nil {
		return KaminoPrimeUSDCRequest{}, fmt.Errorf("unknown PRIME/USDC leg")
	}
	route, err := runtimeRoute(RouteID)
	if err != nil {
		return KaminoPrimeUSDCRequest{}, err
	}
	key, _ := kaminoPolicyLeg(route, leg)
	policy, err := policies.account(key)
	if err != nil {
		return KaminoPrimeUSDCRequest{}, err
	}
	for _, binding := range m.RuntimeBindings.PrimeUSDC.Packets {
		if binding.Action != action {
			continue
		}
		data, err := base64.StdEncoding.Strict().DecodeString(binding.DataBase64)
		if err != nil || len(data) != 16 || !bytesEqual(data[:8], discriminator) || readU64(data[8:]) != 0 {
			continue
		}
		binary.LittleEndian.PutUint64(data[8:], amount)
		request := KaminoPrimeUSDCRequest{
			Action: action, AmountRaw: amount, Policy: policy,
			Accounts: binding.Accounts, Data: data, RecentBlockhash: blockhash.Blockhash,
			LastValidBlockHeight: blockhash.LastValidBlockHeight,
		}
		if _, observedLeg, err := kaminoPrimeUSDCInstruction(request); err == nil && observedLeg == leg {
			return request, nil
		}
	}
	return KaminoPrimeUSDCRequest{}, ErrBridgePrerequisitesUnavailable
}

// kaminoPacketForRoute is lane's packet for leg, for amount, executed through
// the leg's installed policy.
func (m RouteManifest) kaminoPacketForRoute(policies installedPolicies, action Action, leg kaminoPrimeUSDCLeg, amount uint64, blockhash LatestBlockhash, lane string) (KaminoPrimeUSDCRequest, error) {
	if lane == "" || lane == RouteID {
		request, err := m.primeUSDCPacket(policies, action, leg, amount, blockhash)
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
	discriminator := kaminoLegDiscriminator(leg)
	if discriminator == nil {
		return KaminoPrimeUSDCRequest{}, ErrBridgePrerequisitesUnavailable
	}
	key, _ := kaminoPolicyLeg(route, leg)
	policy, err := policies.account(key)
	if err != nil {
		return KaminoPrimeUSDCRequest{}, err
	}
	deposit, borrow, repay, withdraw := kaminoMetasForRoute(route)
	metas := map[kaminoPrimeUSDCLeg][]accountMeta{kaminoLegDeposit: deposit, kaminoLegBorrow: borrow, kaminoLegRepay: repay, kaminoLegWithdraw: withdraw}[leg]
	accounts := make(KaminoPrimeUSDCAccounts, len(metas))
	for i, item := range metas {
		accounts[i].Address, accounts[i].Signer, accounts[i].Writable = encodeBase58(item.key[:]), item.signer, item.writable
	}
	data := binary.LittleEndian.AppendUint64(append([]byte(nil), discriminator...), amount)
	request := KaminoPrimeUSDCRequest{Action: action, AmountRaw: amount, Policy: policy, Accounts: accounts, Data: data,
		RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight, RouteLane: lane}
	if _, observedLeg, err := kaminoRouteInstruction(request, lane); err != nil || observedLeg != leg {
		return KaminoPrimeUSDCRequest{}, ErrBridgePrerequisitesUnavailable
	}
	return request, nil
}

func (m RouteManifest) executionBlocker() *RuntimeBlocker {
	if m.Status != "ready" || m.hasPhaseOneUnresolved() {
		return ErrBridgePrerequisitesUnavailable
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
		// The observation lane authority is the same reviewed manifest lane
		// set that admits entries and initializer decisions: installed lanes
		// and the candidate AUTO lane.
		if !m.selectorObservation || !selectorOrAutoLane(m.observationLane) {
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
