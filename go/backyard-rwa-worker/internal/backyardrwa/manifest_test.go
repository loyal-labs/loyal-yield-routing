package backyardrwa

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedManifestIsExactCheckedInManifest(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "manifests", "backyard-rwa-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source, embeddedBackyardManifest) {
		t.Fatal("embedded runtime manifest drifted from docs/manifests/backyard-rwa-v1.json")
	}
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.executionBlocker() != nil {
		t.Fatal("installed Phase 1 manifest remained blocked")
	}
}

func TestManifestPacketTemplatePatchesOnlyTheV2Amount(t *testing.T) {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	data := append(append([]byte(nil), kaminoDepositCollateral...), make([]byte, 8)...)
	overlay, err := json.Marshal(map[string]any{"packets": []any{map[string]any{
		"action": OpenPrimeUSDCStep, "policy": bridgeAllocationPolicy,
		"policyAccountDataSha256": "11" + string(bytes.Repeat([]byte{'1'}, 62)),
		"policyConstraintIndex":   0, "accounts": manifestAccounts(kaminoDepositMetas()),
		"dataBase64": base64.StdEncoding.EncodeToString(data),
	}}})
	if err != nil || json.Unmarshal(overlay, &manifest.RuntimeBindings.PrimeUSDC) != nil {
		t.Fatal("could not create packet fixture")
	}
	request, err := manifest.primeUSDCPacket(OpenPrimeUSDCStep, kaminoLegDeposit, 77, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 9})
	if err != nil || request.AmountRaw != 77 || readU64(request.Data[8:]) != 77 || !bytes.Equal(request.Data[:8], kaminoDepositCollateral) {
		t.Fatalf("request=%+v err=%v", request, err)
	}
}

func TestManifestRollsOnlyForwardJupiterPolicy(t *testing.T) {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	forward, err := manifest.jupiterPolicy(SwapUSDCToPrimeStep)
	if err != nil {
		t.Fatal(err)
	}
	reverse, err := manifest.jupiterPolicy(SwapPrimeToUSDCStep)
	if err != nil {
		t.Fatal(err)
	}
	if forward.Policy != "FZjjJScy689WWSwhwr2HZPy2aevZukq75niD6gW3b1TG" ||
		forward.PolicyAccountDataSHA256 != "fdc11ac8e9226feef4db8d30065035fde00d6f2eb9a7f940f6ebffa869962d72" ||
		len(forward.ConstraintBindings) != 2 {
		t.Fatal("forward Jupiter action is not bound to the exact seed-66 policy")
	}
	if reverse.Policy != "Fks3YBQWBYA1d6ZZKEAEunjhVMXZA9gY7vfWUWWbQtDx" ||
		reverse.PolicyAccountDataSHA256 != "6cdf12f0cd4623d60b32dc6d58b655e1fcbddf82ae7f75cd7b12783087b9ecc7" ||
		reverse.PolicyConstraintIndex != 1 || len(reverse.ConstraintBindings) != 0 {
		t.Fatal("reverse Jupiter action drifted from the legacy policy constraint")
	}
}
