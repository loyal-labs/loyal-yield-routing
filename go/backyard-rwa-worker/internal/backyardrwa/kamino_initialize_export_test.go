package backyardrwa

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
	for i, lane := range []string{PhaseOneLaneID, SelectedRouteID, "OnRe/ONyc/USDC"} {
		r := KaminoInitializationRequest{RouteLane: lane, PolicySeed: uint64(141 + i), PolicyAccountDataSHA256: sha256Bytes([]byte("local candidate policy; hash does not enter wire")), RecentBlockhash: bridgeVault, LastValidBlockHeight: 100, RentLamports: 17637760, MaximumFeeLamports: 5000}
		message, err := CompileKaminoInitializationMessage(r)
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, map[string]any{"lane": lane, "request": r, "messageBase64": base64.StdEncoding.EncodeToString(message), "messageSha256": sha256Bytes(message), "singleSignerPacketBytes": 65 + len(message)})
	}
	out, err := json.MarshalIndent(map[string]any{"schema": "selector-go-initializer-messages/v1", "broadcast": false, "policyProvenance": "synthetic local candidate pins; not production activation", "messages": messages}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(out, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}
