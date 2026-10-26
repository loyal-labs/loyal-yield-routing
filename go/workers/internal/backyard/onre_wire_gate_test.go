package backyard

import (
	"bytes"
	"crypto/ed25519"
	"testing"
)

const onreLane = "OnRe/ONyc/USDC"

// Every OnRe Kamino leg the worker can build, in the reserve topology prepare
// gives it, compiled by the real compiler and run through the persisted-wire
// gate PersistSigned uses. A refusal here is a signed wire the worker cannot
// persist (live 2026-09-28: the AUTO top-up deposit).
func TestOnReKaminoWiresPassThePersistedWireGate(t *testing.T) {
	t.Parallel()
	manifest := embeddedTestManifest(t)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{51}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	route, err := runtimeRoute(onreLane)
	if err != nil {
		t.Fatal(err)
	}
	collateral, debt := route.Kamino.CollateralReserve, route.Kamino.DebtReserve
	for _, c := range []struct {
		name     string
		action   Action
		leg      kaminoLeg
		reserves []string
	}{
		{"initial deposit (flat obligation)", OpenRouteStep, kaminoLegDeposit, []string{}},
		{"top-up deposit (debt-free position)", OpenRouteStep, kaminoLegDeposit, []string{collateral}},
		{"redeposit (leveraged position)", OpenRouteStep, kaminoLegDeposit, []string{collateral, debt}},
		{"borrow", OpenRouteStep, kaminoLegBorrow, []string{collateral}},
		{"repay", DeleverRouteStep, kaminoLegRepay, []string{collateral, debt}},
		{"withdraw with debt (repayment release)", DeleverRouteStep, kaminoLegWithdraw, []string{collateral, debt}},
		{"withdraw debt-free", DeleverRouteStep, kaminoLegWithdraw, []string{collateral}},
	} {
		request, err := manifest.kaminoPacketForRoute(testPolicies(t), c.action, c.leg, 1_000_000, LatestBlockhash{Blockhash: backyardSettings, LastValidBlockHeight: 99}, onreLane)
		if err != nil {
			t.Fatalf("%s: packet: %v", c.name, err)
		}
		request.ObligationReserves = c.reserves
		message, err := compileKaminoMessageForDelegate(rwa, request, delegate)
		if err != nil {
			t.Errorf("%s: compile: %v", c.name, err)
			continue
		}
		if _, _, _, _, err := decodeExactWire(signTestWire(t, key, message)); err != nil {
			t.Errorf("%s: persisted-wire decode refused: %v", c.name, err)
		}
		if err := signedTestBuildResult(t, key, message).validateForDelegate(delegate); err != nil {
			t.Errorf("%s: PersistSigned gate refused: %v", c.name, err)
		}
	}
}
