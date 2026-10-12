package backyard

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
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

func TestAutoQuoteEvidenceThroughInstalledPolicy(t *testing.T) {
	t.Parallel()
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
		!strings.Contains(err.Error(), "no catalog conversion on "+autoAUTOPYUSD.Lane) {
		t.Fatalf("foreign AUTO pair quoted: %v", err)
	}
}

func newJSONBody(value string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(value))
}
