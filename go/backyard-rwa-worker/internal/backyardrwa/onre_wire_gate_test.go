package backyardrwa

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
	manifest := basicPolicyFixtureManifest(t)
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
		leg      kaminoPrimeUSDCLeg
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
		request, err := manifest.kaminoPacketForRoute(c.action, c.leg, 1_000_000, LatestBlockhash{Blockhash: bridgeSettings, LastValidBlockHeight: 99}, onreLane)
		if err != nil {
			t.Fatalf("%s: packet: %v", c.name, err)
		}
		request.ObligationReserves = c.reserves
		message, err := manifest.compileKaminoMessage(request, delegate)
		if err != nil {
			t.Errorf("%s: compile: %v", c.name, err)
			continue
		}
		if _, _, _, _, err := decodeExactLegacyWire(signTestWire(t, key, message)); err != nil {
			t.Errorf("%s: persisted-wire decode refused: %v", c.name, err)
		}
		if err := signedTestBuildResult(t, key, message).validateForDelegate(delegate); err != nil {
			t.Errorf("%s: PersistSigned gate refused: %v", c.name, err)
		}
	}
}

// The installed OnRe initializer compiles with the pinned delegate as payer,
// so only its wire shape is checked here, like the AUTO initializer test.
func TestOnReInitializerWirePassesTheDecodeGate(t *testing.T) {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	request, err := manifest.initializationRequest(onreLane, LatestBlockhash{Blockhash: bridgeSettings, LastValidBlockHeight: 99}, 24_165_120, 5_000)
	if err != nil {
		t.Fatal(err)
	}
	message, err := CompileKaminoInitializationMessage(request)
	if err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{52}, ed25519.SeedSize))
	if _, _, _, signer, err := decodeExactLegacyWire(signTestWire(t, key, message)); err != nil || signer != mustKey(bridgeDelegate) {
		t.Fatalf("OnRe initializer wire refused: %v", err)
	}
}

// The top-up topology stays closed for the other installed lanes.
func TestTopupDepositTopologyOnlyForAUTOAndOnRe(t *testing.T) {
	manifest := basicPolicyFixtureManifest(t)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{53}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	for _, lane := range []string{PhaseOneLaneID, SelectedRouteID} {
		route, _ := runtimeRoute(lane)
		request, err := manifest.kaminoPacketForRoute(OpenRouteStep, kaminoLegDeposit, 1_000_000, LatestBlockhash{Blockhash: bridgeSettings, LastValidBlockHeight: 99}, lane)
		if err != nil {
			t.Fatal(err)
		}
		request.ObligationReserves = []string{route.Kamino.CollateralReserve}
		message, err := manifest.compileKaminoMessage(request, delegate)
		if err != nil {
			t.Fatal(err)
		}
		if err := signedTestBuildResult(t, key, message).validateForDelegate(delegate); err == nil {
			t.Fatalf("%s accepted the collateral-only deposit topology", lane)
		}
	}
}

// Both OnRe swap directions from the recorded Jupiter exports: USDC->ONyc
// stays a legacy packet, ONyc->USDC needs the v0 packet with lookup tables.
// Each signed wire must pass the gate PersistSigned uses.
func TestOnReSwapWiresPassThePersistedWireGate(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{54}, ed25519.SeedSize))
	for _, leg := range []string{"USDC->ONyc", "ONyc->USDC"} {
		request, record := basicJupiterRequestFromExport(t, onreLane, leg)
		if !record.SingleSignerPacketFits {
			request.LookupTables = retainedOrReconstructedLookupTables(t, request.Instruction.LookupTableAddresses, legacyMessageKeys(t, record.MessageBase64), []string{record.PolicyAccount})
		}
		message, err := CompileJupiterMessage(request)
		if err != nil {
			t.Fatalf("%s: compile: %v", leg, err)
		}
		// The compiler pins the production delegate as fee payer, so the
		// shape is checked with the decoder PersistSigned routes to.
		var decodeErr error
		if message[0] == 0x80 {
			_, _, _, _, decodeErr = decodeExactV0Wire(signTestWire(t, key, message))
		} else {
			_, _, _, _, decodeErr = decodeExactLegacyWire(signTestWire(t, key, message))
		}
		if decodeErr != nil {
			t.Errorf("%s (version byte %#x): persisted-wire decode refused: %v", leg, message[0], decodeErr)
		}
	}
}
