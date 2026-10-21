package backyard

import (
	"bytes"
	"crypto/ed25519"
	"testing"
)

// Live 2026-09-28: every plan B3 top-up deposit (OPEN_ROUTE_STEP into the
// funded debt-free AUTO obligation) failed PersistSigned. Prepare lists the
// obligation's one collateral reserve, so the wire refreshes that reserve;
// the persisted-wire gate must accept exactly that deposit topology.
func TestTopupDepositWirePassesThePersistedWireGate(t *testing.T) {
	t.Parallel()
	manifest := embeddedTestManifest(t)
	delegateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{41}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(delegateKey.Public().(ed25519.PublicKey))
	route := autoAUTOPYUSD
	for name, reserves := range map[string][]string{
		"initial deposit (flat obligation)":    {},
		"top-up deposit (collateral, no debt)": {route.Kamino.CollateralReserve},
		"redeposit (collateral and debt)":      {route.Kamino.CollateralReserve, route.Kamino.DebtReserve},
	} {
		request, err := manifest.kaminoPacketForRoute(testPolicies(t), OpenRouteStep, kaminoLegDeposit, 36_000_000, LatestBlockhash{Blockhash: backyardSettings, LastValidBlockHeight: 99}, route.Lane)
		if err != nil {
			t.Fatal(err)
		}
		request.ObligationReserves = reserves
		message, err := compileKaminoMessageForDelegate(rwa, request, delegate)
		if err != nil {
			t.Fatalf("%s: compile: %v", name, err)
		}
		if _, _, _, _, err := decodeExactWire(signTestWire(t, delegateKey, message)); err != nil {
			t.Errorf("%s: persisted-wire decode refused: %v", name, err)
		}
		if err := signedTestBuildResult(t, delegateKey, message).validateForDelegate(delegate); err != nil {
			t.Errorf("%s: PersistSigned gate refused: %v", name, err)
		}
	}
}
