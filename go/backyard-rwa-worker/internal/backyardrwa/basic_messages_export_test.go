package backyardrwa

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

type basicMessageAccount struct {
	Pubkey     string `json:"pubkey"`
	IsSigner   bool   `json:"isSigner"`
	IsWritable bool   `json:"isWritable"`
}

type basicMessageInstruction struct {
	Kind           string                `json:"kind"`
	ProgramID      string                `json:"programId"`
	Accounts       []basicMessageAccount `json:"accounts"`
	DataBase64     string                `json:"dataBase64"`
	EncodedInOuter bool                  `json:"encodedInOuter,omitempty"`
}

type basicMessageRecord struct {
	Lane                    string                    `json:"lane"`
	Leg                     string                    `json:"leg"`
	Kind                    string                    `json:"kind"`
	PolicyFamily            BasicPolicyFamily         `json:"policyFamily"`
	PolicySeed              uint64                    `json:"policySeed"`
	ConstraintIndex         byte                      `json:"constraintIndex"`
	PolicyAccount           string                    `json:"policyAccount"`
	PolicyDataSHA256        string                    `json:"policyDataSha256"`
	PolicyDataSHA256Source  string                    `json:"policyDataSha256Source"`
	AmountRaw               uint64                    `json:"amountRaw"`
	RecentBlockhash         string                    `json:"recentBlockhash"`
	LastValidBlockHeight    int64                     `json:"lastValidBlockHeight"`
	MessageBase64           string                    `json:"messageBase64"`
	MessageSHA256           string                    `json:"messageSha256"`
	Instructions            []basicMessageInstruction `json:"instructions"`
	AccountsToLoad          []string                  `json:"accountsToLoad"`
	JupiterFixtureSource    string                    `json:"jupiterFixtureSource,omitempty"`
	SingleSignerPacketBytes int                       `json:"singleSignerPacketBytes"`
	SingleSignerPacketFits  bool                      `json:"singleSignerPacketFits"`
	WorkerCompileError      string                    `json:"workerCompileError,omitempty"`
}

type basicMessageExport struct {
	Schema                 string               `json:"schema"`
	Cluster                string               `json:"cluster"`
	Commitment             string               `json:"commitment"`
	ReadOnly               bool                 `json:"readOnly"`
	Broadcast              bool                 `json:"broadcast"`
	PolicySeeds            map[string]uint64    `json:"policySeeds"`
	PolicyDataSHA256Source string               `json:"policyDataSha256Source"`
	JupiterFixtureSource   string               `json:"jupiterFixtureSource"`
	PreExistingFailures    []string             `json:"preExistingFailures"`
	Messages               []basicMessageRecord `json:"messages"`
}

type recordedJupiterInstruction struct {
	ProgramID  string                      `json:"programId"`
	Accounts   []JupiterInstructionAccount `json:"accounts"`
	DataBase64 string                      `json:"dataBase64"`
}

func (instruction recordedJupiterInstruction) runtime() JupiterSwapInstruction {
	return JupiterSwapInstruction{ProgramID: instruction.ProgramID, Accounts: instruction.Accounts, Data: instruction.DataBase64}
}

type recordedJupiterHeaders struct {
	Rows []struct {
		Key   string `json:"key"`
		Quote struct {
			InAmountRaw             string `json:"inAmountRaw"`
			OutAmountRaw            string `json:"outAmountRaw"`
			OtherAmountThresholdRaw string `json:"otherAmountThresholdRaw"`
			RoutePlanLength         int    `json:"routePlanLength"`
		} `json:"quote"`
		Instruction recordedJupiterInstruction `json:"instruction"`
	} `json:"rows"`
}

const basicJupiterFixturePath = "../../../../docs/evidence/backyard-rwa-go/policy-jupiter-headers-v1.json"

func basicPolicyFixtureHash(manifest RouteManifest, family BasicPolicyFamily) string {
	var hash *string
	switch family {
	case BasicCollateralLifecycle:
		hash = manifest.RuntimeBindings.CollateralLifecycle.DataSHA256
	case BasicDebtLifecycle:
		hash = manifest.RuntimeBindings.DebtLifecycle.DataSHA256
	case BasicSwapRoutesA:
		hash = manifest.RuntimeBindings.SwapRoutesA.DataSHA256
	case BasicSwapRoutesB:
		hash = manifest.RuntimeBindings.SwapRoutesB.DataSHA256
	}
	if hash == nil {
		return ""
	}
	return *hash
}

func basicMessageInstructionFromCompiled(kind string, instruction compiledInstruction, encodedInOuter bool) basicMessageInstruction {
	accounts := make([]basicMessageAccount, len(instruction.accounts))
	for index, account := range instruction.accounts {
		accounts[index] = basicMessageAccount{Pubkey: encodeBase58(account.key[:]), IsSigner: account.signer, IsWritable: account.writable}
	}
	return basicMessageInstruction{Kind: kind, ProgramID: encodeBase58(instruction.program[:]), Accounts: accounts, DataBase64: base64.StdEncoding.EncodeToString(instruction.data), EncodedInOuter: encodedInOuter}
}

func basicAccountsToLoad(instructions []basicMessageInstruction) []string {
	set := map[string]bool{}
	for _, instruction := range instructions {
		set[instruction.ProgramID] = true
		for _, account := range instruction.Accounts {
			set[account.Pubkey] = true
		}
	}
	accounts := make([]string, 0, len(set))
	for account := range set {
		accounts = append(accounts, account)
	}
	sort.Strings(accounts)
	return accounts
}

func basicMessageRecordFromBytes(lane, leg, kind string, family BasicPolicyFamily, policy BasicPolicyBinding, hash string, amount uint64, blockhash LatestBlockhash, message []byte, instructions []basicMessageInstruction) basicMessageRecord {
	digest := sha256.Sum256(message)
	return basicMessageRecord{Lane: lane, Leg: leg, Kind: kind, PolicyFamily: family, PolicySeed: policy.Seed,
		ConstraintIndex: policy.Index[leg], PolicyAccount: policy.Policy, PolicyDataSHA256: hash,
		PolicyDataSHA256Source: "offline fixture hash; replace with finalized policy readback before runtime",
		AmountRaw:              amount, RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight,
		MessageBase64: base64.StdEncoding.EncodeToString(message), MessageSHA256: hex.EncodeToString(digest[:]),
		Instructions: instructions, AccountsToLoad: basicAccountsToLoad(instructions),
		SingleSignerPacketBytes: 1 + ed25519.SignatureSize + len(message), SingleSignerPacketFits: 1+ed25519.SignatureSize+len(message) <= solanaPacketBytes}
}

func exportBasicKaminoMessage(t *testing.T, manifest RouteManifest, lane string, action Action, leg kaminoPrimeUSDCLeg, name string) basicMessageRecord {
	t.Helper()
	amount := uint64(1_000_000)
	blockhash := LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}
	request, err := manifest.kaminoPacketForRoute(action, leg, amount, blockhash, lane)
	if err != nil {
		t.Fatal(err)
	}
	route, err := runtimeRoute(lane)
	if err != nil {
		t.Fatal(err)
	}
	inner, resolvedLeg, err := kaminoResolvedRouteInstruction(request, route)
	if err != nil || resolvedLeg != leg {
		t.Fatalf("resolve %s: %v", name, err)
	}
	outer, err := wrapSquadsKaminoPolicy(mustKey(request.Policy), mustKey(bridgeDelegate), mustKey(bridgeDelegate), request.PolicyConstraintIndex, inner)
	if err != nil {
		t.Fatal(err)
	}
	compiled := append(kaminoRefreshInstructionsForResolvedRoute(leg, request, route), outer)
	instructions := make([]basicMessageInstruction, 0, len(compiled))
	for index, instruction := range compiled {
		kind := "kamino_refresh_reserve"
		if index == 2 {
			kind = "kamino_refresh_obligation"
		} else if index == 3 {
			kind = "squads_execute_transaction_sync"
		}
		instructions = append(instructions, basicMessageInstructionFromCompiled(kind, instruction, false))
	}
	message, err := CompileKaminoMessage(request)
	if err != nil {
		t.Fatal(err)
	}
	family := basicPolicyFamilyForKaminoLeg(leg)
	policy, err := basicPolicyBinding(family)
	if err != nil {
		t.Fatal(err)
	}
	record := basicMessageRecordFromBytes(lane, name, "kamino", family, policy, basicPolicyFixtureHash(manifest, family), amount, blockhash, message, instructions)
	return record
}

func exportBasicJupiterMessage(t *testing.T, manifest RouteManifest, lane string, action Action, row struct {
	Key   string `json:"key"`
	Quote struct {
		InAmountRaw             string `json:"inAmountRaw"`
		OutAmountRaw            string `json:"outAmountRaw"`
		OtherAmountThresholdRaw string `json:"otherAmountThresholdRaw"`
		RoutePlanLength         int    `json:"routePlanLength"`
	} `json:"quote"`
	Instruction recordedJupiterInstruction `json:"instruction"`
}) basicMessageRecord {
	var quotePlan []json.RawMessage
	for index := 0; index < row.Quote.RoutePlanLength; index++ {
		quotePlan = append(quotePlan, json.RawMessage(`{"swapInfo":{"label":"recorded-policy-header"}}`))
	}
	quote := JupiterQuote{InAmount: row.Quote.InAmountRaw, OutAmount: row.Quote.OutAmountRaw, OtherAmountThreshold: row.Quote.OtherAmountThresholdRaw, SwapMode: "ExactIn", SlippageBPS: 50, PlatformFee: json.RawMessage("null"), RoutePlan: quotePlan}
	quote.InputMint, quote.OutputMint, _, _, _ = jupiterEdgeForRoute(action, lane)
	amount, err := parseUint(quote.InAmount)
	if err != nil {
		t.Fatal(err)
	}
	out, minimum, err := validateJupiterQuoteForRoute(quote, action, amount, lane)
	if err != nil {
		t.Fatalf("quote %s: %v", row.Key, err)
	}
	binding, err := manifest.jupiterPolicyForRoute(action, lane)
	if err != nil {
		t.Fatal(err)
	}
	blockhash := LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}
	request := JupiterSwapRequest{Action: action, AmountRaw: amount, QuotedOutputRaw: out, MinimumOutputRaw: minimum,
		Policy: binding.Policy, PolicyAccountDataSHA256: binding.PolicyAccountDataSHA256, PolicyConstraintIndex: binding.PolicyConstraintIndex,
		Instruction: row.Instruction.runtime(), RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight, RouteLane: lane}
	_, workerCompileErr := CompileJupiterMessage(request)
	if workerCompileErr != nil && !strings.Contains(workerCompileErr.Error(), "unsigned message does not fit") {
		t.Fatalf("worker compile %s: %v", row.Key, workerCompileErr)
	}
	inner, err := validateJupiterInstructionForRoute(row.Instruction.runtime(), action, amount, out, minimum, lane)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := wrapSquadsJupiterPolicy(mustKey(binding.Policy), mustKey(bridgeDelegate), mustKey(bridgeDelegate), binding.PolicyConstraintIndex, inner)
	if err != nil {
		t.Fatal(err)
	}
	message, err := compileLegacyMessage(mustKey(bridgeDelegate), mustKey(blockhash.Blockhash), []compiledInstruction{outer})
	if err != nil {
		t.Fatalf("compile %s: %v", row.Key, err)
	}
	instructions := []basicMessageInstruction{
		basicMessageInstructionFromCompiled("squads_execute_transaction_sync", outer, false),
		basicMessageInstructionFromCompiled("jupiter_shared_accounts_route", inner, true),
	}
	family := BasicSwapRoutesA
	if action == SwapCollateralToStableStep {
		family = BasicSwapRoutesB
	}
	policy, err := basicPolicyBinding(family)
	if err != nil {
		t.Fatal(err)
	}
	record := basicMessageRecordFromBytes(lane, row.Key, "jupiter", family, policy, basicPolicyFixtureHash(manifest, family), amount, blockhash, message, instructions)
	record.ConstraintIndex = binding.PolicyConstraintIndex
	if workerCompileErr != nil {
		record.WorkerCompileError = workerCompileErr.Error()
	}
	record.JupiterFixtureSource = basicJupiterFixturePath
	return record
}

func parseUint(value string) (uint64, error) {
	var parsed uint64
	if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil {
		return 0, err
	}
	return parsed, nil
}

func TestExportBasicGoMessages(t *testing.T) {
	manifest := basicPolicyFixtureManifest(t)
	var fixture recordedJupiterHeaders
	data, err := os.ReadFile(basicJupiterFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	rows := map[string]struct {
		lane   string
		action Action
	}{
		"USDC->PRIME":     {PhaseOneLaneID, SwapStableToCollateralStep},
		"PRIME->USDC":     {PhaseOneLaneID, SwapCollateralToStableStep},
		"USDC->syrupUSDC": {SelectedRouteID, SwapStableToCollateralStep},
		"syrupUSDC->USDC": {SelectedRouteID, SwapCollateralToStableStep},
		"USDC->ONyc":      {"OnRe/ONyc/USDC", SwapStableToCollateralStep},
		"ONyc->USDC":      {"OnRe/ONyc/USDC", SwapCollateralToStableStep},
	}
	byKey := map[string]struct {
		Key   string `json:"key"`
		Quote struct {
			InAmountRaw             string `json:"inAmountRaw"`
			OutAmountRaw            string `json:"outAmountRaw"`
			OtherAmountThresholdRaw string `json:"otherAmountThresholdRaw"`
			RoutePlanLength         int    `json:"routePlanLength"`
		} `json:"quote"`
		Instruction recordedJupiterInstruction `json:"instruction"`
	}{}
	for _, row := range fixture.Rows {
		byKey[row.Key] = row
	}
	records := make([]basicMessageRecord, 0, 18)
	for _, lane := range []string{PhaseOneLaneID, SelectedRouteID, "OnRe/ONyc/USDC"} {
		records = append(records,
			exportBasicKaminoMessage(t, manifest, lane, OpenRouteStep, kaminoLegDeposit, "deposit"),
			exportBasicKaminoMessage(t, manifest, lane, OpenRouteStep, kaminoLegBorrow, "borrow"),
			exportBasicKaminoMessage(t, manifest, lane, DeleverRouteStep, kaminoLegRepay, "repay"),
			exportBasicKaminoMessage(t, manifest, lane, DeleverRouteStep, kaminoLegWithdraw, "withdraw"),
		)
		for _, key := range []string{"USDC->PRIME", "PRIME->USDC"} {
			if lane == SelectedRouteID {
				key = map[bool]string{true: "USDC->syrupUSDC", false: "syrupUSDC->USDC"}[key == "USDC->PRIME"]
			} else if lane == "OnRe/ONyc/USDC" {
				key = map[bool]string{true: "USDC->ONyc", false: "ONyc->USDC"}[key == "USDC->PRIME"]
			}
			plan := rows[key]
			records = append(records, exportBasicJupiterMessage(t, manifest, lane, plan.action, byKey[key]))
		}
	}
	export := basicMessageExport{Schema: "loyal-backyard-rwa-basic-go-messages/v1", Cluster: "mainnet-beta", Commitment: "confirmed", ReadOnly: true, Broadcast: false,
		PolicySeeds:            map[string]uint64{"CollateralLifecycle": 141, "DebtLifecycle": 142, "SwapRoutesA": 143, "SwapRoutesB": 144},
		PolicyDataSHA256Source: "offline fixture hash; replace with finalized policy readback before runtime",
		JupiterFixtureSource:   basicJupiterFixturePath,
		PreExistingFailures:    []string{"TestPhase3ReturnQuoteCompatibility", "TestPolicySetupCreatedStateMatchesSDKAndRejectsAuthorityDrift"}, Messages: records}
	output, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	output = append(output, '\n')
	if err := os.WriteFile("../../../../docs/evidence/backyard-rwa-basic/go-messages-v1.json", output, 0o644); err != nil {
		t.Fatal(err)
	}
}
