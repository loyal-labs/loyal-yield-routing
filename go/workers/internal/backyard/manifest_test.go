package backyard

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

func TestEmbeddedManifestIsExecutable(t *testing.T) {
	t.Parallel()
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	if blocker := manifest.executionBlocker(); blocker != nil {
		t.Fatalf("v2 manifest is blocked after the policy install readback: %v", blocker)
	}
}

func TestManifestPacketTemplatePatchesOnlyTheV2Amount(t *testing.T) {
	t.Parallel()
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	data := append(append([]byte(nil), kamino.DepositV2Discriminator[:]...), make([]byte, 8)...)
	overlay, err := json.Marshal(map[string]any{"packets": []any{map[string]any{
		"action": OpenPrimeUSDCStep, "accounts": manifestAccounts(kaminoLegMetasForRoute(kaminoLegDeposit, RouteID)),
		"dataBase64": base64.StdEncoding.EncodeToString(data),
	}}})
	if err != nil || json.Unmarshal(overlay, &manifest.RuntimeBindings.PrimeUSDC) != nil {
		t.Fatal("could not create packet fixture")
	}
	request, err := manifest.primeUSDCPacket(testPolicies(t), OpenPrimeUSDCStep, kaminoLegDeposit, 77, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 9})
	if err != nil || request.AmountRaw != 77 || readU64(request.Data[8:]) != 77 || !bytes.Equal(request.Data[:8], kamino.DepositV2Discriminator[:]) {
		t.Fatalf("request=%+v err=%v", request, err)
	}
}
