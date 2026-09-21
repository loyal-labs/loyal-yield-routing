package backyardrwa

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// TestExportAutoExecutionMessages writes the exact unsigned AUTO execution
// messages the LiteSVM proof executes verbatim. Local proof input only: no
// signer, no RPC, no deployment manifest. Every message is compiled through the
// superseding seed-901 initializer binding so the policy account in each wire
// is the eight-constraint candidate the Rust harness installs, and the blockhash
// slot carries the settings sentinel the harness convention uses.
//
// Coverage is the complete worker-generated surface: all four Kamino lifecycle
// legs, all five approved Jupiter swap directions, and the Multiply
// initializer. Amounts are raw units of the leg's own mint (deposit/withdraw
// move the AUTO collateral mint; borrow/repay and the PYUSD sides of swaps move
// the PYUSD debt mint); the reviewed docs pin no AUTO collateral decimals, so
// no fiat-style scaling is invented here.
func TestExportAutoExecutionMessages(t *testing.T) {
	path := os.Getenv("AUTO_EXECUTION_GO_OUTPUT")
	if path == "" {
		t.Skip("requires AUTO_EXECUTION_GO_OUTPUT for connected local proof")
	}
	manifest := autoInitializerFixtureManifest(t)
	binding := autoInitializerFixtureBinding(t)
	delegate := mustKey(bridgeDelegate)
	messages := make([]map[string]any, 0, 10)

	addMessage := func(kind, leg string, constraintIndex byte, amountRaw any, request any, message []byte) {
		keys, instructions := decodeResourceTestMessage(t, message)
		messages = append(messages, map[string]any{
			"kind":                    kind,
			"leg":                     leg,
			"lane":                    autoAUTOPYUSD.Lane,
			"constraintIndex":         constraintIndex,
			"amountRaw":               amountRaw,
			"policyAccount":           binding.Policy,
			"request":                 request,
			"messageBase64":           base64.StdEncoding.EncodeToString(message),
			"messageSha256":           sha256Bytes(message),
			"instructionCount":        len(instructions),
			"accountsToLoad":          keys,
			"singleSignerPacketBytes": 65 + len(message),
		})
	}

	blockhash := LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}

	// The four Kamino lifecycle legs, at the proven per-leg constraint
	// indexes. Deposit/borrow are open-side legs; repay/withdraw are
	// delever-side legs — the exact action each packet must carry.
	lifecycle := []struct {
		leg      kaminoPrimeUSDCLeg
		action   Action
		kind     string
		legName  string
		amount   uint64
		indexKey string
	}{
		{kaminoLegDeposit, OpenRouteStep, "kamino-deposit", "deposit", 5_000_000, "deposit"},
		{kaminoLegBorrow, OpenRouteStep, "kamino-borrow", "borrow", 1_000_000, "borrow"},
		{kaminoLegRepay, DeleverRouteStep, "kamino-repay", "repay", 1_000_000, "repay"},
		{kaminoLegWithdraw, DeleverRouteStep, "kamino-withdraw", "withdraw", 5_000_000, "withdraw"},
	}
	for _, item := range lifecycle {
		request, err := manifest.kaminoPacketForRoute(item.action, item.leg, item.amount, blockhash, autoAUTOPYUSD.Lane)
		if err != nil {
			t.Fatalf("%s: %v", item.kind, err)
		}
		if request.PolicyConstraintIndex != binding.ConstraintIndices[item.indexKey] {
			t.Fatalf("%s constraint index drifted: %d", item.kind, request.PolicyConstraintIndex)
		}
		message, err := manifest.compileKaminoMessage(request, delegate)
		if err != nil {
			t.Fatalf("%s: %v", item.kind, err)
		}
		addMessage(item.kind, item.legName, request.PolicyConstraintIndex, item.amount, request, message)
	}

	// The five approved Jupiter swap directions, at the proven biclique cover
	// indexes: both into-AUTO edges share index 4, both out-of-AUTO edges share
	// index 5, and PYUSD->USDC is index 6.
	swaps := []struct {
		action   Action
		kind     string
		legName  string
		amount   uint64
		out      uint64
		indexKey string
	}{
		{SwapStableToCollateralStep, "jupiter-usdc-to-auto", "USDC to AUTO custody", 1_000_000, 5_000_000, "swapUSDCOrPYUSDToAUTO"},
		{SwapDebtToCollateralStep, "jupiter-pyusd-to-auto", "PYUSD to AUTO custody", 1_000_000, 5_000_000, "swapUSDCOrPYUSDToAUTO"},
		{SwapCollateralToStableStep, "jupiter-auto-to-usdc", "AUTO custody to USDC", 5_000_000, 1_000_000, "swapAUTOToUSDCOrPYUSD"},
		{SwapCollateralToDebtStep, "jupiter-auto-to-pyusd", "AUTO custody to PYUSD", 5_000_000, 1_000_000, "swapAUTOToUSDCOrPYUSD"},
		{SwapDebtToUSDCStep, "jupiter-pyusd-to-usdc", "PYUSD custody to USDC", 1_000_000, 990_000, "swapPYUSDToUSDC"},
	}
	for _, item := range swaps {
		jupiter := autoJupiterTestRequest(t, item.action, item.amount, item.out, 0)
		jupiter.Policy = binding.Policy
		jupiter.PolicyAccountDataSHA256 = binding.AccountDataSHA256
		jupiter.PolicyConstraintIndex = binding.ConstraintIndices[item.indexKey]
		message, err := manifest.compileJupiterMessage(jupiter, delegate)
		if err != nil {
			t.Fatalf("%s: %v", item.kind, err)
		}
		addMessage(item.kind, item.legName, jupiter.PolicyConstraintIndex, item.amount, jupiter, message)
	}

	// The Multiply initializer: canonical heap frame + the vault-signed
	// initializer outer.
	initializerRequest := KaminoInitializationRequest{RouteLane: autoAUTOPYUSD.Lane, PolicySeed: binding.PolicySeed,
		PolicyAccountDataSHA256: binding.AccountDataSHA256, RecentBlockhash: bridgeVault,
		LastValidBlockHeight: 100, RentLamports: 17_637_760, MaximumFeeLamports: 5000}
	initializerMessage, err := manifest.compileKaminoInitializationMessage(initializerRequest)
	if err != nil {
		t.Fatal(err)
	}
	addMessage("initializer", "initializeObligation", binding.ConstraintIndices[autoInitializerConstraintKey], nil, initializerRequest, initializerMessage)

	// The export must carry exactly the ten worker-generated messages — no
	// silent omission of a leg or direction, no duplication.
	expected := []struct {
		kind  string
		index byte
	}{
		{"kamino-deposit", 0}, {"kamino-borrow", 2}, {"kamino-repay", 3}, {"kamino-withdraw", 1},
		{"jupiter-usdc-to-auto", 4}, {"jupiter-pyusd-to-auto", 4},
		{"jupiter-auto-to-usdc", 5}, {"jupiter-auto-to-pyusd", 5}, {"jupiter-pyusd-to-usdc", 6},
		{"initializer", 7},
	}
	if len(messages) != len(expected) {
		t.Fatalf("export carries %d messages, expected exactly %d", len(messages), len(expected))
	}
	for index, want := range expected {
		got := messages[index]
		if got["kind"] != want.kind || got["constraintIndex"] != want.index {
			t.Fatalf("export position %d is %v/%v, expected %s/%d", index, got["kind"], got["constraintIndex"], want.kind, want.index)
		}
	}

	out, err := json.MarshalIndent(map[string]any{
		"schema":           "loyal-backyard-rwa-auto-execution-go-messages/v1",
		"broadcast":        false,
		"policyProvenance": "superseding seed-901 candidate binding; fixtures only, not production activation",
		"coverage":         "all four Kamino lifecycle legs, all five approved swap directions, and the initializer — the complete worker-generated surface",
		"messages":         messages,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(out, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("wrote %s: %d messages\n", path, len(messages))
}
