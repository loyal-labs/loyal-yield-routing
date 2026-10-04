package fleetexec

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

// TestGenerateSignedWireFixture deterministically writes the committed
// fixture. It is skipped unless FLEETEXEC_WRITE_FIXTURES=1 so normal runs
// only consume the provenance-pinned bytes.
func TestGenerateSignedWireFixture(t *testing.T) {
	if os.Getenv("FLEETEXEC_WRITE_FIXTURES") != "1" {
		t.Skip("fixture regeneration is explicit")
	}
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(7*i + 3)
	}
	key := ed25519.NewKeyFromSeed(seed)
	feePayer := base58.Encode(key[32:])
	secondary := base58.Encode(rotate(seed, 11))
	blockhash := base58.Encode(rotate(seed, 23))

	// v0 message: header (1 required signer, 0 readonly signed, 1 readonly
	// unsigned), two static keys, blockhash, one instruction addressed to
	// the readonly program with the fee payer writable.
	message := []byte{0x80, 1, 0, 1, 2}
	message = append(message, key[32:]...)
	message = append(message, rotate(seed, 11)...)
	message = append(message, rotate(seed, 23)...)
	message = append(message, 1, 1, 1, 0, 1, 0xAA, 0)

	unsigned := append([]byte{0x01}, make([]byte, 64)...)
	unsigned = append(unsigned, message...)
	signature := ed25519.Sign(key, message)
	signed := append([]byte{0x01}, signature...)
	signed = append(signed, message...)
	unsignedHash := sha256.Sum256(unsigned)
	messageHash := sha256.Sum256(message)
	signedHash := sha256.Sum256(signed)

	fixture := signedWireFixture{
		Description:      "Delegated fee-payer signing identity for fleet v0 route wire; deterministic test key, no chain value.",
		FeePayerSeedB58:  base58.Encode(seed),
		FeePayer:         feePayer,
		RecentBlockhash:  blockhash,
		MessageB64:       base64.StdEncoding.EncodeToString(message),
		UnsignedWireB64:  base64.StdEncoding.EncodeToString(unsigned),
		MessageSHA256:    hex.EncodeToString(messageHash[:]),
		UnsignedWireSHA:  hex.EncodeToString(unsignedHash[:]),
		SignedWireB64:    base64.StdEncoding.EncodeToString(signed),
		SignedWireSHA256: hex.EncodeToString(signedHash[:]),
		SignatureB58:     base58.Encode(signature),
		LastValidHeight:  4_000,
		FeeLamports:      5_000,
		ComputeLimit:     200_000,
		LookupTable:      base58.Encode(rotate(seed, 31)),
		SecondaryAccount: secondary,
	}
	raw, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixturePath, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rotate(seed []byte, offset byte) []byte {
	out := make([]byte, len(seed))
	for i := range seed {
		out[i] = seed[i] + offset
	}
	return out
}
