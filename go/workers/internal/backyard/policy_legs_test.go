package backyard

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

// Every leg a runtime literal executes is built by its production compiler
// and executes at the constraint of its literal that admits it, and at no
// other: the leg constants and the literals are proven against each other,
// through the bytes a worker signs.
func TestEveryRuntimeLegExecutesAtItsOwnConstraint(t *testing.T) {
	manifest := embeddedTestManifest(t)
	policies := testPolicies(t)
	literals, err := backyardPolicies()
	if err != nil {
		t.Fatal(err)
	}
	byAccount := map[solana.PublicKey]policyKey{}
	for key := range literals {
		if policies[key].matches == 1 {
			byAccount[policies[key].Account] = key
		}
	}
	blockhash := LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}
	type row struct {
		name  string
		build func(t *testing.T) ([]byte, []LookupTableSnapshot)
	}
	var rows []row
	for _, lane := range []string{autoAUTOPYUSD.Lane, PhaseOneLaneID, SelectedRouteID, onreONycUSDC, ethenaUSDePYUSD.Lane, primePRIMEPYUSD.Lane, primePRIMEUSDS.Lane, RouteID} {
		for _, leg := range kaminoLegs {
			open, delever := OpenRouteStep, DeleverRouteStep
			if lane == RouteID {
				open, delever = OpenPrimeUSDCStep, DeleverPrimeUSDCStep
			}
			action := open
			if leg == kaminoLegRepay || leg == kaminoLegWithdraw {
				action = delever
			}
			rows = append(rows, row{fmt.Sprintf("kamino/%s/%s", lane, leg), func(t *testing.T) ([]byte, []LookupTableSnapshot) {
				request, err := manifest.kaminoPacketForRoute(policies, action, leg, 1_000_000, blockhash, lane)
				if err != nil {
					t.Fatal(err)
				}
				message, err := CompileKaminoMessage(request)
				if err != nil {
					t.Fatal(err)
				}
				return message, nil
			}})
		}
	}
	for _, lane := range []string{autoAUTOPYUSD.Lane, PhaseOneLaneID, SelectedRouteID, onreONycUSDC} {
		rows = append(rows, row{"initialize/" + lane, func(t *testing.T) ([]byte, []LookupTableSnapshot) {
			r, err := manifest.initializationRequest(policies, lane, blockhash, 17_637_760, 5000)
			if err != nil {
				t.Fatal(err)
			}
			message, err := manifest.compileKaminoInitializationMessage(r)
			if err != nil {
				t.Fatal(err)
			}
			return message, nil
		}})
	}
	for _, action := range []Action{VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV} {
		rows = append(rows, row{"bridge/" + string(action), func(t *testing.T) ([]byte, []LookupTableSnapshot) {
			amount := uint64(1_000_000)
			if action == ReportNAV {
				amount = 0
			}
			message, err := CompileBridgeMessage(bridgeTestRequest(action, amount))
			if err != nil {
				t.Fatal(err)
			}
			return message, nil
		}})
	}
	for _, action := range []Action{SwapStableToCollateralStep, SwapDebtToCollateralStep, SwapCollateralToStableStep, SwapCollateralToDebtStep, SwapDebtToUSDCStep} {
		rows = append(rows, row{"swap/" + autoAUTOPYUSD.Lane + "/" + string(action), func(t *testing.T) ([]byte, []LookupTableSnapshot) {
			return compileTestSwap(t, autoJupiterTestRequest(t, action, 1_000_000, 990_000, 0))
		}})
	}
	for plan, id := range primeForwardPlanIDs {
		rows = append(rows, row{fmt.Sprintf("swap/%s/%s/plan-%d", RouteID, SwapUSDCToPrimeStep, plan), func(t *testing.T) ([]byte, []LookupTableSnapshot) {
			request := legacyPrimeSwapRequest(SwapUSDCToPrimeStep)
			data, _ := base64.StdEncoding.DecodeString(request.Instruction.Data)
			data[8] = id
			request.Instruction.Data = base64.StdEncoding.EncodeToString(data)
			return compileTestSwap(t, request)
		}})
	}
	rows = append(rows, row{fmt.Sprintf("swap/%s/%s", RouteID, SwapPrimeToUSDCStep), func(t *testing.T) ([]byte, []LookupTableSnapshot) {
		return compileTestSwap(t, legacyPrimeSwapRequest(SwapPrimeToUSDCStep))
	}})
	for lane, legs := range map[string][2]string{PhaseOneLaneID: {"USDC->PRIME", "PRIME->USDC"}, SelectedRouteID: {"USDC->syrupUSDC", "syrupUSDC->USDC"}, onreONycUSDC: {"USDC->ONyc", "ONyc->USDC"}} {
		for _, leg := range legs {
			rows = append(rows, row{"swap/" + lane + "/" + leg, func(t *testing.T) ([]byte, []LookupTableSnapshot) {
				request, record := basicJupiterRequestFromExport(t, lane, leg)
				if _, err := CompileJupiterMessage(request); err != nil && len(request.Instruction.LookupTableAddresses) > 0 {
					request.LookupTables = retainedOrReconstructedLookupTables(t, request.Instruction.LookupTableAddresses, legacyMessageKeys(t, record.MessageBase64), []string{record.PolicyAccount})
				}
				return compileTestSwap(t, request)
			}})
		}
	}
	for _, lane := range []string{ethenaUSDePYUSD.Lane, primePRIMEPYUSD.Lane, primePRIMEUSDS.Lane} {
		for _, action := range []Action{SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep} {
			rows = append(rows, row{"swap/" + lane + "/" + string(action), func(t *testing.T) ([]byte, []LookupTableSnapshot) {
				return compileTestSwap(t, catalogSwapRequest(t, lane, action))
			}})
		}
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			message, tables := r.build(t)
			execution := decodeTestExecution(t, message, tables)
			key, ok := byAccount[execution.Policy]
			if !ok {
				t.Fatalf("executes through %s, which is no installed literal", execution.Policy)
			}
			for i, ix := range execution.Inner {
				var admitting []int
				for leg, constraint := range literals[key].Constraints {
					if squads.Admits(constraint, ix, vaultObligation) {
						admitting = append(admitting, leg)
					}
				}
				if want := []int{int(execution.ConstraintIndexes[i])}; fmt.Sprint(admitting) != fmt.Sprint(want) && !sharedSwapLeg(key, admitting, want[0]) {
					t.Errorf("%s: instruction %d executes at constraint %d, but constraints %v admit it", key, i, want[0], admitting)
				}
			}
		})
	}
}

// sharedSwapLeg is the one documented overlap: a basic swap family admits
// PRIME under both of its legs, and PRIME swaps under the first.
func sharedSwapLeg(key policyKey, admitting []int, leg int) bool {
	return (key.family == BasicSwapRoutesA || key.family == BasicSwapRoutesB) &&
		fmt.Sprint(admitting) == fmt.Sprint([]int{basicSwapONycPrime, basicSwapPrimeSyrup}) && leg == basicSwapONycPrime
}

// vaultObligation is the state a KLend leg's obligation predicate reads: an
// obligation owned by KLend whose owner (at byte 64) is the vault.
func vaultObligation(solana.PublicKey) *chain.Account {
	data := make([]byte, kamino.ObligationSize)
	vault := solanaKey(bridgeVault)
	copy(data[64:], vault[:])
	return &chain.Account{Owner: kamino.ProgramID, Data: data}
}

// compileTestSwap is the swap's production message and the lookup tables it
// needs when it does not fit a legacy packet.
func compileTestSwap(t *testing.T, request JupiterSwapRequest) ([]byte, []LookupTableSnapshot) {
	t.Helper()
	message, err := CompileJupiterMessage(request)
	if err == nil {
		return message, request.LookupTables
	}
	if !strings.Contains(err.Error(), "does not fit") {
		t.Fatal(err)
	}
	if len(request.Instruction.LookupTableAddresses) == 0 {
		request.LookupTables = retainedJupiterLookups(t)
	}
	for _, address := range request.Instruction.LookupTableAddresses {
		for _, table := range readRetainedJupiterLookups(t, "prime-sibling-lookup-review-2026-09-05.json", 8) {
			if table.Address == address {
				request.LookupTables = append(request.LookupTables, table)
			}
		}
	}
	message, err = CompileJupiterMessage(request)
	if err != nil {
		t.Fatal(err)
	}
	return message, request.LookupTables
}

// legacyPrimeSwapRequest is the legacy PRIME/USDC route's swap of action
// through its installed policy.
func legacyPrimeSwapRequest(action Action) JupiterSwapRequest {
	return JupiterSwapRequest{Action: action, AmountRaw: 1_000_000, QuotedOutputRaw: 990_000, MinimumOutputRaw: 985_050,
		Policy:      testPolicyAccount(policyKey{lane: RouteID, action: action}),
		Instruction: jupiterTestInstruction(action, 1_000_000, 990_000, false), RecentBlockhash: bridgeSettings, LastValidBlockHeight: 2}
}

// catalogSwapRequest is a catalog lane's swap of action from the retained
// mainnet Jupiter header of its edge, through the policy that edge executes
// under.
func catalogSwapRequest(t *testing.T, lane string, action Action) JupiterSwapRequest {
	t.Helper()
	var headers struct {
		Rows []struct {
			Key          string
			LookupTables []string
			Instruction  struct {
				ProgramID, DataBase64 string
				Accounts              []jupiter.AccountMeta
			}
		}
	}
	raw, err := os.ReadFile(basicJupiterFixturePath)
	if err != nil || json.Unmarshal(raw, &headers) != nil {
		t.Fatal("retained Jupiter headers unavailable", err)
	}
	from, to, _, _, err := jupiterEdgeForRoute(action, lane)
	if err != nil {
		t.Fatal(err)
	}
	symbol := map[string]string{}
	for _, route := range []RuntimeRoute{ethenaUSDePYUSD, primePRIMEPYUSD, primePRIMEUSDS} {
		symbol[route.Kamino.CollateralMint], symbol[route.Kamino.DebtMint] = route.CollateralSymbol, route.DebtSymbol
	}
	symbol[bridgeUSDC] = "USDC"
	key := symbol[from] + "->" + symbol[to]
	for _, row := range headers.Rows {
		if row.Key != key {
			continue
		}
		instruction := JupiterSwapInstruction{ProgramID: row.Instruction.ProgramID, Data: row.Instruction.DataBase64, Accounts: row.Instruction.Accounts}
		if strings.HasPrefix(lane, "Prime/") {
			instruction.LookupTableAddresses = row.LookupTables
		}
		data, err := base64.StdEncoding.Strict().DecodeString(instruction.Data)
		if err != nil {
			t.Fatal(err)
		}
		at := len(data) - jupiter.PlatformFeeAfterInAmount - 1
		amount, out := readU64(data[at:]), readU64(data[at+jupiter.QuotedOutAfterInAmount:])
		policy, _, err := jupiterPolicyLeg(lane, action, data)
		if err != nil {
			t.Fatal(err)
		}
		return JupiterSwapRequest{Action: action, RouteLane: lane, AmountRaw: amount, QuotedOutputRaw: out, MinimumOutputRaw: out,
			Policy: testPolicyAccount(policy), Instruction: instruction, RecentBlockhash: bridgeVault, LastValidBlockHeight: 99}
	}
	t.Fatalf("no retained header for %s", key)
	return JupiterSwapRequest{}
}

// decodeTestExecution reads a compiled message back into its one Squads
// policy execution, resolving lookup tables.
func decodeTestExecution(t *testing.T, message []byte, tables []LookupTableSnapshot) squads.ExecuteSync {
	t.Helper()
	wire := append(append([]byte{1}, make([]byte, 64)...), message...)
	tx, err := solana.TransactionFromBytes(wire)
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) > 0 {
		resolved := map[solana.PublicKey]solana.PublicKeySlice{}
		for _, snapshot := range tables {
			table, err := decodeMessageLookupTable(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			for _, address := range table.addresses {
				resolved[solana.PublicKey(table.key)] = append(resolved[solana.PublicKey(table.key)], solana.PublicKey(address))
			}
		}
		if err := tx.Message.SetAddressTables(resolved); err != nil {
			t.Fatal(err)
		}
		if err := tx.Message.ResolveLookups(); err != nil {
			t.Fatal(err)
		}
	}
	var found []squads.ExecuteSync
	for _, compiled := range tx.Message.Instructions {
		program, err := tx.Message.ResolveProgramIDIndex(compiled.ProgramIDIndex)
		if err != nil {
			t.Fatal(err)
		}
		if program != squads.ProgramID {
			continue
		}
		metas, err := compiled.ResolveInstructionAccounts(&tx.Message)
		if err != nil {
			t.Fatal(err)
		}
		ix := squads.Instruction{ProgramID: program, Data: compiled.Data}
		for _, meta := range metas {
			ix.Accounts = append(ix.Accounts, *meta)
		}
		execution, err := squads.DecodeExecuteTransactionSyncV2(ix)
		if err != nil {
			t.Fatal(err)
		}
		found = append(found, execution)
	}
	if len(found) != 1 {
		t.Fatalf("message carries %d Squads executions, want one", len(found))
	}
	return found[0]
}
