package backyard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// autoVenueKey deterministically derives a non-authority venue account for
// fixture instructions. Patterns never collide with the pinned identities.
func autoVenueKey(index byte) string {
	pattern := bytes.Repeat([]byte{0xA0 ^ index}, 32)
	pattern[0] = index + 1
	return encodeBase58(pattern)
}

// autoJupiterTestInstruction is a v2 swap on one approved AUTO edge
// (v2TestInstruction) with filler venue accounts after its twelve fixed ones.
func autoJupiterTestInstruction(t *testing.T, action Action, amount, out uint64, filler int) JupiterSwapInstruction {
	t.Helper()
	return v2TestInstruction(autoAUTOPYUSD.Lane, action, amount, out, filler)
}

// autoSwapKey is the policy the AUTO lane swaps action through.
func autoSwapKey(t *testing.T, action Action) policyKey {
	t.Helper()
	key, _, err := jupiterPolicyLeg(autoAUTOPYUSD.Lane, action)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// autoSwapLeg is the constraint index the AUTO lane swaps action under.
func autoSwapLeg(t *testing.T, action Action) byte {
	t.Helper()
	_, leg, err := jupiterPolicyLeg(autoAUTOPYUSD.Lane, action)
	if err != nil {
		t.Fatal(err)
	}
	return leg
}

// autoJupiterTestRequest is the swap request on one approved AUTO edge,
// through the AUTO lane's installed policy.
func autoJupiterTestRequest(t *testing.T, action Action, amount, out uint64, filler int) JupiterSwapRequest {
	t.Helper()
	// The honest enforceable minimum for the wire below (data slippage 50): quoted output scaled by (10000-50)/10000, never the
	// advisory quoted output itself.
	return JupiterSwapRequest{Action: action, AmountRaw: amount, QuotedOutputRaw: out, MinimumOutputRaw: out * 9950 / 10000,
		Policy:          testPolicyAccount(autoSwapKey(t, action)),
		Instruction:     autoJupiterTestInstruction(t, action, amount, out, filler),
		RecentBlockhash: bridgeSettings, LastValidBlockHeight: 99, RouteLane: autoAUTOPYUSD.Lane}
}

// autoFixtureLookupTable fabricates one chain-shaped address-lookup-table
// account carrying exactly the supplied venue keys.
func autoFixtureLookupTable(t *testing.T, entries []string) LookupTableSnapshot {
	t.Helper()
	body := make([]byte, 56)
	binary.LittleEndian.PutUint32(body[0:4], 1)
	binary.LittleEndian.PutUint64(body[4:12], ^uint64(0))
	for _, entry := range entries {
		key, err := decodeKey(entry)
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, key[:]...)
	}
	table := LookupTableSnapshot{Address: autoVenueKey(0xEE), Owner: addressLookupTableProgram,
		Lamports: 56960640, Data: body, ObservedSlot: 1_000_000}
	if _, err := decodeMessageLookupTable(table); err != nil {
		t.Fatalf("fabricated lookup table is invalid: %v", err)
	}
	return table
}

func TestAutoOversizedSwapEdgeIsMeasuredAndUsesTheV0EscapeHatch(t *testing.T) {
	manifest := embeddedTestManifest(t)
	delegate := mustKey(bridgeDelegate)
	// Measure where the legacy packet limit is crossed across the validator's
	// full account range, then exercise the exact oversized edge.
	crossed, crossedBytes, maxAccountsBytes := 0, 0, 0
	for filler := 0; filler <= 64-12; filler++ {
		request := autoJupiterTestRequest(t, SwapStableToCollateralStep, 1_000_000, 990_000, filler)
		inner, err := validateJupiterInstructionForRoute(request.Instruction, request.Action, request.AmountRaw, request.QuotedOutputRaw, request.MinimumOutputRaw, request.RouteLane)
		if err != nil {
			t.Fatal(err)
		}
		outer, err := wrapSquadsJupiterPolicy(mustKey(request.Policy), delegate, delegate, autoSwapLeg(t, SwapStableToCollateralStep), inner)
		if err != nil {
			t.Fatal(err)
		}
		message, err := compileLegacyMessage(delegate, mustKey(bridgeSettings), []compiledInstruction{outer})
		if err != nil {
			t.Fatal(err)
		}
		if filler == 20 { // Jupiter is quoted with maxAccounts=32
			maxAccountsBytes = len(message) + 65
		}
		if crossed == 0 && len(message)+65 > solanaPacketBytes {
			crossed, crossedBytes = 12+filler, len(message)+65
		}
	}
	if crossed == 0 {
		t.Fatal("no account count crossed the legacy packet limit; measurement is stale")
	}
	t.Logf("AUTO USDC->AUTO legacy packet: realistic Jupiter maxAccounts=32 quote = %d bytes (fits: %v); limit %d crossed at %d inner accounts (%d bytes)",
		maxAccountsBytes, maxAccountsBytes <= solanaPacketBytes, solanaPacketBytes, crossed, crossedBytes)
	request := autoJupiterTestRequest(t, SwapStableToCollateralStep, 1_000_000, 990_000, 64-12)
	if _, err := compileJupiterMessageForDelegate(request, delegate); err == nil || !strings.Contains(err.Error(), "unsigned message does not fit") {
		t.Fatalf("oversized AUTO edge compiled without hints: %v", err)
	}
	fillers := make([]string, 0, len(request.Instruction.Accounts))
	for index, account := range request.Instruction.Accounts {
		if index < 12 {
			continue // reviewed boundaries and authorities stay static
		}
		fillers = append(fillers, account.Pubkey)
	}
	table := autoFixtureLookupTable(t, fillers)
	request.Instruction.LookupTableAddresses = []string{table.Address}
	request.LookupTables = []LookupTableSnapshot{table}
	message, err := compileJupiterMessageForDelegate(request, delegate)
	if err != nil || message[0] != 0x80 || message[1] != 1 {
		t.Fatalf("validated AUTO hints did not produce a v0 outer message: %v", err)
	}
	if len(message)+65 > solanaPacketBytes {
		t.Fatalf("v0 AUTO packet %d still exceeds the limit", len(message)+65)
	}
	t.Logf("AUTO USDC->AUTO v0 packet with one quoted lookup table = %d bytes (+65 signature = %d of %d)", len(message), len(message)+65, solanaPacketBytes)
	staticKeys, _, outerData := decodeV0OuterInstruction(t, message)
	if !bytes.Equal(outerData[:8], squads.ExecuteTransactionSyncV2Discriminator[:]) {
		t.Fatal("v0 outer instruction left the Squads execute")
	}
	for _, authority := range []string{request.Policy, squads.ProgramID.String(), bridgeDelegate} {
		found := false
		for _, key := range staticKeys {
			found = found || key == authority
		}
		if !found {
			t.Fatalf("v0 message offloaded authority account %s", authority)
		}
	}
	// Without quoted hints the same edge must stay fail-closed, never guess.
	hintless := request
	hintless.LookupTables = nil
	hintless.Instruction.LookupTableAddresses = nil
	if _, err := manifest.prepareJupiterLookupTables(context.Background(), nil, hintless, 1); err == nil ||
		!strings.Contains(err.Error(), "unsupported construction") {
		t.Fatalf("hintless oversized AUTO edge prepared: %v", err)
	}
}

func TestAutoQuoteEvidenceThroughInstalledPolicy(t *testing.T) {
	manifest := embeddedTestManifest(t)
	route, err := runtimeRoute(autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	instruction := autoJupiterTestInstruction(t, SwapStableToCollateralStep, 1_000_000, 990_000, 0)
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var response string
		switch r.URL.Path {
		case "/quote":
			response = `{"inputMint":"` + bridgeUSDC + `","outputMint":"` + route.Kamino.CollateralMint +
				`","inAmount":"1000000","outAmount":"990000","otherAmountThreshold":"985050",` +
				`"swapMode":"ExactIn","slippageBps":50,"platformFee":null,"routePlan":[{}]}`
		case "/swap-instructions":
			body, _ := json.Marshal(map[string]any{"setupInstructions": []any{}, "otherInstructions": []any{},
				"cleanupInstruction": nil, "tokenLedgerInstruction": nil, "swapInstruction": instruction,
				"addressLookupTableAddresses": []any{}})
			response = string(body)
		default:
			t.Errorf("unexpected Jupiter endpoint %s", r.URL.Path)
			return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: newJSONBody(response), Header: make(http.Header)}, nil
	})
	client, err := fixtureJupiter(transport)
	if err != nil {
		t.Fatal(err)
	}
	rpc := newFakeChain(t, nil)
	rpcOf(rpc).Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body struct {
			Method string
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Method != "getLatestBlockhash" {
			t.Fatalf("fitting AUTO quote path read %s; no chain lookup read was expected", body.Method)
		}
		return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":42},"value":{"blockhash":"` + bridgeSettings + `","lastValidBlockHeight":99}}}`), nil
	})
	decision := Decision{Action: SwapStableToCollateralStep, Reason: "candidate_quote", IdempotencyKey: "candidate-quote", AmountRaw: 1_000_000, StrategyKey: autoAUTOPYUSD.Lane}
	evidence, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, manifest, testPolicies(t), decision, 2_000_000, 0, 42)
	if err != nil {
		t.Fatal(err)
	}
	request := evidence.Request
	if request.RouteLane != autoAUTOPYUSD.Lane || request.Policy != testPolicyAccount(autoSwapKey(t, SwapStableToCollateralStep)) {
		t.Fatalf("quote evidence does not execute through the installed AUTO swap policy: %+v", request)
	}
	if request.MinimumOutputRaw != 985_050 || request.QuotedOutputRaw != 990_000 {
		t.Fatalf("quote economics drifted: %+v", request)
	}
	source := evidence.ExpectedEffects.Accounts[0]
	destination := evidence.ExpectedEffects.Accounts[1]
	if source.Address != bridgeSquadsATA || source.AfterRaw != 1_000_000 || source.Owner != classicTokenProgram {
		t.Fatalf("source effect drifted: %+v", source)
	}
	if destination.Address != route.CollateralCustody || destination.AfterRaw != 985_050 {
		t.Fatalf("destination effect drifted: %+v", destination)
	}
	if _, err := compileJupiterMessageForDelegate(request, mustKey(bridgeDelegate)); err != nil {
		t.Fatalf("quote-path request did not compile through the installed policy: %v", err)
	}
	// The same decision without its swap policy installed holds by name.
	swapKey := autoSwapKey(t, SwapStableToCollateralStep)
	uninstalled := installedPolicies{}
	for key, view := range testPolicies(t) {
		if key != swapKey {
			uninstalled[key] = view
		}
	}
	_, err = prepareJupiterQuoteEvidence(context.Background(), rpc, client, manifest, uninstalled, decision, 2_000_000, 0, 42)
	assertBudgetHold(t, err, swapKey.String()+" policy not installed")
	// A foreign action on this lane is rejected before any quote is requested.
	foreign := decision
	foreign.Action = SwapUSDCToDebtStep
	if _, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, manifest, testPolicies(t), foreign, 2_000_000, 0, 42); err == nil ||
		!strings.Contains(err.Error(), "not an approved AUTO") {
		t.Fatalf("foreign AUTO pair quoted: %v", err)
	}
}

func TestAutoQuoteEvidencePreparesChainLookupTablesForOversizedEdges(t *testing.T) {
	manifest := embeddedTestManifest(t)
	route, err := runtimeRoute(autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	instruction := autoJupiterTestInstruction(t, SwapStableToCollateralStep, 1_000_000, 990_000, 64-12)
	table := autoFixtureLookupTable(t, nil)
	for index := 10; index < len(instruction.Accounts); index++ {
		table.Data = append(table.Data, mustDecodeKey(t, instruction.Accounts[index].Pubkey)...)
	}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var response string
		switch r.URL.Path {
		case "/quote":
			response = `{"inputMint":"` + bridgeUSDC + `","outputMint":"` + route.Kamino.CollateralMint +
				`","inAmount":"1000000","outAmount":"990000","otherAmountThreshold":"985050",` +
				`"swapMode":"ExactIn","slippageBps":50,"platformFee":null,"routePlan":[{}]}`
		case "/swap-instructions":
			body, _ := json.Marshal(map[string]any{"setupInstructions": []any{}, "otherInstructions": []any{},
				"cleanupInstruction": nil, "tokenLedgerInstruction": nil, "swapInstruction": instruction,
				"addressLookupTableAddresses": []string{table.Address}})
			response = string(body)
		default:
			t.Errorf("unexpected Jupiter endpoint %s", r.URL.Path)
			return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: newJSONBody(response), Header: make(http.Header)}, nil
	})
	client, err := fixtureJupiter(transport)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	rpc := newFakeChain(t, nil)
	rpcOf(rpc).Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body struct {
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		switch body.Method {
		case "getLatestBlockhash":
			return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":42},"value":{"blockhash":"` + bridgeSettings + `","lastValidBlockHeight":99}}}`), nil
		case "getMultipleAccounts":
			reads++
			var addresses []string
			if err := json.Unmarshal(body.Params[0], &addresses); err != nil || len(addresses) != 1 || addresses[0] != table.Address {
				t.Fatalf("lookup read left the quoted identity: %v %v", addresses, err)
			}
			value := []map[string]any{{"owner": table.Owner, "lamports": table.Lamports, "executable": false,
				"data": []string{base64.StdEncoding.EncodeToString(table.Data), "base64"}}}
			return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":43},"value":` + marshalTestJSON(t, value) + `}}`), nil
		default:
			t.Fatalf("unexpected RPC read %s", body.Method)
			return nil, nil
		}
	})
	decision := Decision{Action: SwapStableToCollateralStep, Reason: "candidate_quote", IdempotencyKey: "candidate-quote-oversized", AmountRaw: 1_000_000, StrategyKey: autoAUTOPYUSD.Lane}
	evidence, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, manifest, testPolicies(t), decision, 2_000_000, 0, 42)
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 || len(evidence.Request.LookupTables) != 1 {
		t.Fatalf("oversized AUTO quote did not resolve the quoted table: reads=%d tables=%v", reads, evidence.Request.LookupTables)
	}
	message, err := compileJupiterMessageForDelegate(evidence.Request, mustKey(bridgeDelegate))
	if err != nil || message[0] != 0x80 || len(message)+65 > solanaPacketBytes {
		t.Fatalf("quote-path v0 packet did not compile: %v", err)
	}
}

func mustDecodeKey(t *testing.T, value string) []byte {
	t.Helper()
	key, err := decodeKey(value)
	if err != nil {
		t.Fatal(err)
	}
	return key[:]
}

func newJSONBody(value string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(value))
}

func marshalTestJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
