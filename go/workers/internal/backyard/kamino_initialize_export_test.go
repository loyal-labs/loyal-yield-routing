package backyard

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
)

// Local proof input only: never uses a signer, RPC or deployment manifest.
func TestExportKaminoInitializationMessages(t *testing.T) {
	path := os.Getenv("SELECTOR_INITIALIZER_GO_OUTPUT")
	if path == "" {
		t.Skip("requires SELECTOR_INITIALIZER_GO_OUTPUT for connected local proof")
	}
	messages := make([]map[string]any, 0, 3)
	for _, lane := range []string{PhaseOneLaneID, SelectedRouteID, "OnRe/ONyc/USDC"} {
		r := KaminoInitializationRequest{RouteLane: lane, Policy: testPolicyAccount(policyKey{lane: lane, action: InitializeKaminoObligation}), RecentBlockhash: bridgeVault, LastValidBlockHeight: 100, RentLamports: 17637760, MaximumFeeLamports: 5000}
		message, err := CompileKaminoInitializationMessage(r)
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, map[string]any{"lane": lane, "request": r, "messageBase64": base64.StdEncoding.EncodeToString(message), "messageSha256": sha256Bytes(message), "singleSignerPacketBytes": 65 + len(message)})
	}
	out, err := json.MarshalIndent(map[string]any{"schema": "selector-go-initializer-messages/v1", "broadcast": false, "policyProvenance": "the installed initializer policies", "messages": messages}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(out, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}
