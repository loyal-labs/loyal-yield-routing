package fleetexec

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/mr-tron/base58"
)

func TestSignPreparedRouteReproducesFixtureWireExactly(t *testing.T) {
	fixture := mustSignedFixture(t)
	preparation, key, err := fixturePreparation(fixture)
	if err != nil {
		t.Fatalf("prepare fixture: %v", err)
	}
	wire, err := DelegateSigner{FeePayer: key}.SignPreparedRoute(preparation, fixture.LastValidHeight)
	if err != nil {
		t.Fatalf("sign prepared route: %v", err)
	}
	if want, _ := base64.StdEncoding.DecodeString(fixture.SignedWireB64); string(wire.SignedTransaction) != string(want) {
		t.Fatalf("signed wire differs from the fixture bytes")
	}
	if wire.SignedTransactionHash != fixture.SignedWireSHA256 {
		t.Fatalf("signed wire hash %s, want %s", wire.SignedTransactionHash, fixture.SignedWireSHA256)
	}
	if wire.MessageHash != fixture.MessageSHA256 {
		t.Fatalf("message hash %s, want the prepared %s", wire.MessageHash, fixture.MessageSHA256)
	}
	if wire.TransactionSignature != fixture.SignatureB58 {
		t.Fatalf("signature %s, want fixture %s", wire.TransactionSignature, fixture.SignatureB58)
	}
	if wire.RecentBlockhash != fixture.RecentBlockhash {
		t.Fatalf("recent blockhash %s, want prepared %s", wire.RecentBlockhash, fixture.RecentBlockhash)
	}
	if wire.LastValidBlockHeight != fixture.LastValidHeight {
		t.Fatalf("last valid block height %d, want %d", wire.LastValidBlockHeight, fixture.LastValidHeight)
	}
}

func TestSignPreparedRouteRejectsWrongAuthorityAndTamperedEvidence(t *testing.T) {
	fixture := mustSignedFixture(t)
	preparation, key, err := fixturePreparation(fixture)
	if err != nil {
		t.Fatal(err)
	}
	otherSeed := make([]byte, 32)
	for i := range otherSeed {
		otherSeed[i] = byte(0x40 + i)
	}
	wrongSigner := ed25519.NewKeyFromSeed(otherSeed)
	if _, err := (DelegateSigner{FeePayer: wrongSigner}).SignPreparedRoute(preparation, fixture.LastValidHeight); err == nil ||
		!strings.Contains(err.Error(), "not the delegated signer") {
		t.Fatalf("wrong delegated signer error = %v", err)
	}
	tampered := preparation
	tampered.Message = append([]byte(nil), preparation.Message...)
	tampered.Message[len(tampered.Message)-1] ^= 0xFF
	if _, err := (DelegateSigner{FeePayer: key}).SignPreparedRoute(tampered, fixture.LastValidHeight); err == nil ||
		!strings.Contains(err.Error(), "hash does not match") {
		t.Fatalf("tampered message error = %v", err)
	}
	if _, err := (DelegateSigner{FeePayer: key}).SignPreparedRoute(preparation, 0); err == nil {
		t.Fatalf("missing block height must be rejected")
	}
	shortPacket := preparation
	shortPacket.PacketBytes = len(preparation.UnsignedWire) - 1
	if _, err := (DelegateSigner{FeePayer: key}).SignPreparedRoute(shortPacket, fixture.LastValidHeight); err == nil ||
		!strings.Contains(err.Error(), "packet") {
		t.Fatalf("packet bound mismatch error = %v", err)
	}
	oversized := preparation
	oversized.PacketBytes = SolanaPacketLimit + 1
	if _, err := (DelegateSigner{FeePayer: key}).SignPreparedRoute(oversized, fixture.LastValidHeight); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized packet error = %v", err)
	}
	twoSigners := preparation
	twoSigners.Message = append([]byte(nil), preparation.Message...)
	twoSigners.Message[1] = 2 // header demands two offline signatures
	messageHash := sha256.Sum256(twoSigners.Message)
	twoSigners.MessageSHA256 = hex.EncodeToString(messageHash[:])
	unsigned := append([]byte{0x01}, make([]byte, 64)...)
	unsigned = append(unsigned, twoSigners.Message...)
	wireHash := sha256.Sum256(unsigned)
	twoSigners.UnsignedWire = unsigned
	twoSigners.WireSHA256 = hex.EncodeToString(wireHash[:])
	twoSigners.PacketBytes = len(unsigned)
	if _, err := (DelegateSigner{FeePayer: key}).SignPreparedRoute(twoSigners, fixture.LastValidHeight); err == nil ||
		!strings.Contains(err.Error(), "at most the delegate") {
		t.Fatalf("multi-signer error = %v", err)
	}
	if _, err := (DelegateSigner{}).SignPreparedRoute(preparation, fixture.LastValidHeight); err == nil {
		t.Fatalf("missing signing key must be rejected")
	}
}

func TestSignPreparedRoutePreservesMessageBytes(t *testing.T) {
	fixture := mustSignedFixture(t)
	preparation, key, err := fixturePreparation(fixture)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := DelegateSigner{FeePayer: key}.SignPreparedRoute(preparation, fixture.LastValidHeight)
	if err != nil {
		t.Fatal(err)
	}
	// The signed wire is signature-slot + untouched message, so the message
	// the fleet planner simulated is byte-identical to what is broadcast.
	if string(wire.SignedTransaction[65:]) != string(preparation.Message) {
		t.Fatalf("signed wire message differs from the prepared message")
	}
	var canonical struct {
		Wire string `json:"wire"`
	}
	raw, _ := json.Marshal(map[string]string{"wire": base58.Encode(wire.SignedTransaction)})
	if err := json.Unmarshal(raw, &canonical); err != nil || canonical.Wire == "" {
		t.Fatalf("wire identity must survive canonical evidence encoding: %v", err)
	}
}

func TestLegalTransitionGuard(t *testing.T) {
	legal := map[SubmissionState][]SubmissionState{
		StateSigned:                {StateConfirmed, StateExpired, StateFailed},
		StateSubmitted:             {StateConfirmed, StateExpired, StateFailed},
		StateExpiryCheckPending:    {StateConfirmed, StateExpired, StateFailed},
		StateConfirmed:             {StateReconciliationPending},
		StateReconciliationPending: {StateReconciled, StateFailed},
	}
	for from, targets := range legal {
		for _, to := range targets {
			if !legalTransition(from, to) {
				t.Fatalf("%s -> %s must be legal", from, to)
			}
			for other, others := range legal {
				if other == from {
					continue
				}
				for _, notAllowed := range others {
					if notAllowed == to || legalTransition(from, notAllowed) {
						continue
					}
				}
			}
		}
	}
	forbidden := []struct{ from, to SubmissionState }{
		{StateReconciled, StateSubmitted},
		{StateExpired, StateSigned},
		{StateFailed, StateSubmitted},
		{StateSigned, StateReconciled},

		{StateSubmitted, StateReconciled},
		{StateConfirmed, StateSubmitted},
		{StateConfirmed, StateReconciled},
		{StateReconciliationPending, StateEffectAmbiguous},
		{StateSubmitted, StateEffectAmbiguous},
		{StateEffectAmbiguous, StateExpired},
	}
	for _, edge := range forbidden {
		if legalTransition(edge.from, edge.to) {
			t.Fatalf("%s -> %s must be forbidden", edge.from, edge.to)
		}
	}
}

func TestParseBalanceAnchorsRequiresCompleteEvidence(t *testing.T) {
	valid := BalanceAnchorEvidence{
		AccountAddresses: []string{"payer", "source", "target"},
		Anchors: []EffectAnchor{
			{Account: "source", Mint: "usdc", ExpectedDelta: 501_835_024, Decimals: 6},
			{Account: "target", Mint: "usdc", ExpectedDelta: -501_835_024, Decimals: 6},
		},
	}
	raw, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseBalanceAnchors(raw); err != nil {
		t.Fatalf("valid anchors rejected: %v", err)
	}
	broken := map[string]func(*BalanceAnchorEvidence){
		"no addresses":  func(e *BalanceAnchorEvidence) { e.AccountAddresses = nil },
		"no anchors":    func(e *BalanceAnchorEvidence) { e.Anchors = nil },
		"zero delta":    func(e *BalanceAnchorEvidence) { e.Anchors[0].ExpectedDelta = 0 },
		"no mint":       func(e *BalanceAnchorEvidence) { e.Anchors[1].Mint = "" },
		"duplicate":     func(e *BalanceAnchorEvidence) { e.Anchors[1] = e.Anchors[0] },
		"empty account": func(e *BalanceAnchorEvidence) { e.Anchors[0].Account = "" },
	}
	for name, mutate := range broken {
		candidate := valid
		mutate(&candidate)
		raw, _ := json.Marshal(candidate)
		if _, err := ParseBalanceAnchors(raw); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
	if _, err := ParseBalanceAnchors(nil); err == nil {
		t.Fatalf("missing anchor evidence must be rejected")
	}
}

var _ = fleet.PreparedTransaction{}
