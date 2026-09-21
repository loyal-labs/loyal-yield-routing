package backyardrwa

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

// Replay public sizing quotes through the actual production validators. A
// passing test means the compatibility measurement is sound, NOT that the
// return path works: each rejected edge is retained for the sole verifier.
func TestPhase3ReturnQuoteCompatibility(t *testing.T) {
	bytes, err := os.ReadFile("../../../../docs/evidence/backyard-rwa-go/phase3/return-quote-feasibility-2026-09-05.json")
	if err != nil {
		t.Skipf("phase3 return-quote feasibility artifact was never committed (%v); skipping until it is regenerated", err)
	}
	var artifact struct {
		Schema                                                             string
		Broadcast, SignatureProof, ExecutionProof, InstalledPolicyReadback bool
		Rows                                                               []struct {
			Key, DataSHA256 string
			Quote           JupiterQuote
			Response        struct{ SwapInstruction JupiterSwapInstruction }
		}
	}
	if err := json.Unmarshal(bytes, &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.Schema != "phase3-return-quote-feasibility/v1" || artifact.Broadcast || artifact.SignatureProof || artifact.ExecutionProof || artifact.InstalledPolicyReadback || len(artifact.Rows) != 3 {
		t.Fatal("invalid read-only quote evidence")
	}
	actions := map[string]Action{"USDe->PYUSD": SwapCollateralToDebtStep, "PYUSD->USDC": SwapDebtToUSDCStep, "USDe->USDC": SwapCollateralToStableStep}
	seen := map[string]bool{}
	for _, row := range artifact.Rows {
		action, ok := actions[row.Key]
		if !ok || seen[row.Key] {
			t.Fatal("missing, duplicate or foreign return edge")
		}
		seen[row.Key] = true
		amount, err := strconv.ParseUint(row.Quote.InAmount, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		out, minimum, err := validateJupiterQuoteForRoute(row.Quote, action, amount, "Ethena/USDe/PYUSD")
		if err != nil {
			t.Fatal(err)
		}
		data, err := base64.StdEncoding.Strict().DecodeString(row.Response.SwapInstruction.Data)
		if err != nil || sha256Bytes(data) != row.DataSHA256 {
			t.Fatal("quote instruction hash drift")
		}
		_, validationErr := validateJupiterInstructionForRoute(row.Response.SwapInstruction, action, amount, out, minimum, "Ethena/USDe/PYUSD")
		accepted := validationErr == nil
		// These retained V1 quotes have exact public account/economic boundaries,
		// but two have different legacy tail offsets from the installed catalog.
		if accepted != (row.Key == "PYUSD->USDC") {
			t.Fatalf("%s installed-layout result changed: %v", row.Key, validationErr)
		}
		binding, err := catalogJupiterBindingForRoute(action, "Ethena/USDe/PYUSD")
		if err != nil {
			t.Fatal(err)
		}
		reason := ""
		if validationErr != nil {
			reason = validationErr.Error()
		}
		measurement, err := json.Marshal(map[string]any{
			"edge": row.Key, "accepted": accepted, "reason": reason, "artifactSha256": sha256Bytes(bytes),
			"policy": binding.Policy, "policyDataSha256": binding.PolicySHA256, "constraintIndex": binding.ConstraintIndex,
			"installedAmountOffset": binding.AmountOffset, "observedAmountOffset": len(data) - 19,
			"installedSlippageOffset": binding.SlippageOffset, "observedSlippageOffset": len(data) - 3,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("PHASE3_RETURN_QUOTE %s", measurement)
	}
}
