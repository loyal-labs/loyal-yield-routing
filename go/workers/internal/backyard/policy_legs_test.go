package backyard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
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
	swaps := map[string][]Action{autoAUTOPYUSD.Lane: {SwapStableToCollateralStep, SwapDebtToCollateralStep, SwapCollateralToStableStep, SwapCollateralToDebtStep, SwapDebtToUSDCStep},
		RouteID: {SwapUSDCToPrimeStep, SwapPrimeToUSDCStep}}
	for _, lane := range []string{PhaseOneLaneID, SelectedRouteID, onreONycUSDC} {
		swaps[lane] = []Action{SwapStableToCollateralStep, SwapCollateralToStableStep}
	}
	for _, lane := range []string{ethenaUSDePYUSD.Lane, primePRIMEPYUSD.Lane, primePRIMEUSDS.Lane} {
		swaps[lane] = []Action{SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep}
	}
	for lane, actions := range swaps {
		for _, action := range actions {
			rows = append(rows, row{"swap/" + lane + "/" + string(action), func(t *testing.T) ([]byte, []LookupTableSnapshot) {
				return compileTestSwap(t, fixtureSwapRequest(t, lane, action))
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
				if ix.ProgramID != jupiter.ProgramID {
					continue
				}
				// A swap's output stays in the vault and pays no fee.
				foreign := solanaKey(bridgeDelegate) // not a vault custody
				for name, mutate := range map[string]func(*squads.Instruction){
					"foreign destination": func(ix *squads.Instruction) { ix.Accounts[5].PublicKey = foreign },
					"platform fee":        func(ix *squads.Instruction) { ix.Data[jupiter.V2FeesOffset] = 1 },
					"positive slippage":   func(ix *squads.Instruction) { ix.Data[jupiter.V2FeesOffset+2] = 1 },
				} {
					changed := squads.Instruction{ProgramID: ix.ProgramID, Accounts: slices.Clone(ix.Accounts), Data: slices.Clone(ix.Data)}
					mutate(&changed)
					for leg, constraint := range literals[key].Constraints {
						if squads.Admits(constraint, changed, vaultObligation) {
							t.Errorf("%s: constraint %d admits a swap with a %s", key, leg, name)
						}
					}
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

// compileTestSwap is the swap's production message and, when it does not fit
// a legacy packet, the lookup tables it then needs: the swap API's tables,
// standing in here for chain reads by holding the swap's own accounts. A lane
// that takes no lookup tables (the PRIME/USDC route) holds such a swap in
// production; its v0 message is compiled here directly, so its constraint is
// still proven.
func compileTestSwap(t *testing.T, request JupiterSwapRequest) ([]byte, []LookupTableSnapshot) {
	t.Helper()
	message, err := CompileJupiterMessage(request)
	if err == nil {
		return message, request.LookupTables
	}
	if !strings.Contains(err.Error(), "does not fit") {
		t.Fatal(err)
	}
	var accounts []string
	for _, account := range request.Instruction.Accounts {
		accounts = append(accounts, account.Pubkey)
	}
	if len(request.Instruction.LookupTableAddresses) == 0 {
		inner, err := validateJupiterInstructionForRoute(request.Instruction, request.Action, request.AmountRaw, request.QuotedOutputRaw, request.MinimumOutputRaw, request.RouteLane)
		if err != nil {
			t.Fatal(err)
		}
		_, index, err := jupiterPolicyLeg(request.RouteLane, request.Action)
		if err != nil {
			t.Fatal(err)
		}
		outer, err := wrapSquadsJupiterPolicy(mustKey(request.Policy), mustKey(bridgeDelegate), mustKey(bridgeDelegate), index, inner)
		if err != nil {
			t.Fatal(err)
		}
		tables := []LookupTableSnapshot{autoFixtureLookupTable(t, accounts)}
		message, err := compileV0Message(mustKey(bridgeDelegate), mustKey(request.RecentBlockhash), []compiledInstruction{outer}, tables)
		if err != nil {
			t.Fatal(err)
		}
		return message, tables
	}
	for _, address := range request.Instruction.LookupTableAddresses {
		table := autoFixtureLookupTable(t, accounts)
		table.Address, accounts = address, nil
		request.LookupTables = append(request.LookupTables, table)
	}
	message, err = CompileJupiterMessage(request)
	if err != nil {
		t.Fatal(err)
	}
	return message, request.LookupTables
}

// jupiterV2FixturePath holds a live V2 /swap-instructions answer for the
// Backyard vault on every edge a Backyard swap policy admits.
const jupiterV2FixturePath = "testdata/jupiter-v2-swap-instructions.json"

type recordedJupiterInstruction struct {
	ProgramID  string                `json:"programId"`
	Accounts   []jupiter.AccountMeta `json:"accounts"`
	DataBase64 string                `json:"dataBase64"`
}

type jupiterV2Row struct {
	Key   string `json:"key"`
	Quote struct {
		InAmountRaw             string `json:"inAmountRaw"`
		OutAmountRaw            string `json:"outAmountRaw"`
		OtherAmountThresholdRaw string `json:"otherAmountThresholdRaw"`
		RoutePlanLength         int    `json:"routePlanLength"`
	} `json:"quote"`
	QuoteResponse json.RawMessage            `json:"quoteResponse"`
	Instruction   recordedJupiterInstruction `json:"instruction"`
	LookupTables  []string                   `json:"lookupTables"`
}

// jupiterV2Fixture is the fixture's row for the swap of action on lane.
func jupiterV2Fixture(t testing.TB, lane string, action Action) jupiterV2Row {
	t.Helper()
	var fixture struct{ Rows []jupiterV2Row }
	raw, err := os.ReadFile(jupiterV2FixturePath)
	if err != nil || json.Unmarshal(raw, &fixture) != nil {
		t.Fatal("v2 swap fixture unavailable", err)
	}
	from, to, _, _, err := jupiterEdgeForRoute(action, lane)
	if err != nil {
		t.Fatal(err)
	}
	a, err := routeSwapAssets()
	if err != nil {
		t.Fatal(err)
	}
	symbol := map[string]string{}
	for _, asset := range []swapAsset{a.usdc, a.prime, a.usds, a.pyusd, a.auto, a.usde, a.syrup, a.onyc, a.usdg} {
		symbol[asset.mint.String()] = asset.symbol
	}
	key := symbol[from] + "->" + symbol[to]
	for _, row := range fixture.Rows {
		if row.Key == key {
			return row
		}
	}
	t.Fatalf("no v2 fixture row for %s", key)
	return jupiterV2Row{}
}

// fixtureSwapRequest is the request the production builder makes for the
// swap of action on lane from the fixture's quote and instruction, through
// the policy the swap executes under, at the floor its wire enforces.
func fixtureSwapRequest(t *testing.T, lane string, action Action) JupiterSwapRequest {
	t.Helper()
	row := jupiterV2Fixture(t, lane, action)
	client, err := fixtureJupiter(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/quote" {
			if r.URL.Query().Get("instructionVersion") != "V2" {
				t.Error("quote asks for another instruction version")
			}
			return response(string(row.QuoteResponse)), nil
		}
		var asked struct {
			UserPublicKey     string
			UseSharedAccounts bool
		}
		if json.NewDecoder(r.Body).Decode(&asked) != nil || asked.UserPublicKey != bridgeVault || !asked.UseSharedAccounts {
			t.Error("swap-instructions asks for another user or route")
		}
		body, err := json.Marshal(map[string]any{"swapInstruction": map[string]any{"programId": row.Instruction.ProgramID,
			"accounts": row.Instruction.Accounts, "data": row.Instruction.DataBase64}, "addressLookupTableAddresses": row.LookupTables})
		return response(string(body)), err
	}))
	if err != nil {
		t.Fatal(err)
	}
	amount, err := strconv.ParseUint(row.Quote.InAmountRaw, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	quote, instruction, err := freshSwapForRoute(context.Background(), client, lane, action, amount)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := validateJupiterQuoteForRoute(quote, action, amount, lane)
	if err != nil {
		t.Fatal(err)
	}
	floor, err := jupiterInstructionWireFloor(instruction)
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := jupiterPolicyLeg(lane, action)
	if err != nil {
		t.Fatal(err)
	}
	return JupiterSwapRequest{Action: action, RouteLane: lane, AmountRaw: amount, QuotedOutputRaw: out, MinimumOutputRaw: floor,
		Policy: testPolicyAccount(key), Instruction: instruction, RecentBlockhash: bridgeVault, LastValidBlockHeight: 99}
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
