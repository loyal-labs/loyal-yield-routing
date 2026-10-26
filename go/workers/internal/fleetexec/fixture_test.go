package fleetexec

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/mr-tron/base58"
)

// signedWireFixture pins the exact delegated signing contract with fixed
// bytes: a v0 message compiled with one required (fee payer) signature slot,
// its prepared evidence, and the exact wire the signer must produce.
type signedWireFixture struct {
	Description      string `json:"description"`
	FeePayerSeedB58  string `json:"fee_payer_seed_base58"`
	FeePayer         string `json:"fee_payer"`
	RecentBlockhash  string `json:"recent_blockhash"`
	MessageB64       string `json:"message_base64"`
	UnsignedWireB64  string `json:"unsigned_wire_base64"`
	MessageSHA256    string `json:"message_sha256"`
	UnsignedWireSHA  string `json:"unsigned_wire_sha256"`
	SignedWireB64    string `json:"signed_wire_base64"`
	SignedWireSHA256 string `json:"signed_wire_sha256"`
	SignatureB58     string `json:"signature_base58"`
	LastValidHeight  int64  `json:"last_valid_block_height"`
	FeeLamports      uint64 `json:"fee_lamports"`
	ComputeLimit     uint64 `json:"compute_limit"`
	LookupTable      string `json:"lookup_table"`
	SecondaryAccount string `json:"secondary_account"`
}

const fixturePath = "../../testdata/fleetexec/signed_wire.json"

func mustSignedFixture(t *testing.T) signedWireFixture {
	t.Helper()
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture signedWireFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return fixture
}

func fixturePreparation(fixture signedWireFixture) (fleet.PreparedTransaction, ed25519.PrivateKey, error) {
	message, err := base64.StdEncoding.DecodeString(fixture.MessageB64)
	if err != nil {
		return fleet.PreparedTransaction{}, nil, err
	}
	unsigned, err := base64.StdEncoding.DecodeString(fixture.UnsignedWireB64)
	if err != nil {
		return fleet.PreparedTransaction{}, nil, err
	}
	seed, err := base58.Decode(fixture.FeePayerSeedB58)
	if err != nil {
		return fleet.PreparedTransaction{}, nil, err
	}
	key := ed25519.NewKeyFromSeed(seed)
	preparation := fleet.PreparedTransaction{
		Message:          message,
		UnsignedWire:     unsigned,
		MessageSHA256:    fixture.MessageSHA256,
		WireSHA256:       fixture.UnsignedWireSHA,
		LookupTables:     []string{fixture.LookupTable},
		WritableAccounts: []string{fixture.FeePayer, fixture.SecondaryAccount},
		PacketBytes:      len(unsigned),
		FeeLamports:      fixture.FeeLamports,
		ComputeLimit:     fixture.ComputeLimit,
	}
	return preparation, key, nil
}
