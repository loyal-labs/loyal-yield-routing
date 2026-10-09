package backyard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/big"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// Strategy-two cutover gate: the worker must not run while any legacy custom
// policy at seeds 62-65 still exists on the Squads Settings, because those
// policies still delegate to the retired executor this cutover is retiring.
// The four policy addresses are program-derived addresses of
//
//	["smart_account", "policy", bridgeSettings, seed_le64]
//
// under bridgeSquadsProgram, derived at startup from the same constants the
// bridge builder uses (build.go) rather than pinned as literals, so a change
// to the Settings constant re-derives a different gate set instead of
// silently gating the wrong addresses.
var legacyCustomPolicySeeds = []uint64{62, 63, 64, 65}

const mainnetGenesisHash = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"

const legacyPolicyGateTimeout = 60 * time.Second

var squadsSettingsDiscriminator = [8]byte{223, 179, 163, 190, 177, 224, 67, 173}

func legacyCustomPolicyAddresses() ([]string, error) {
	settings, err := decodeKey(bridgeSettings)
	if err != nil {
		return nil, fmt.Errorf("decode bridge settings: %w", err)
	}
	program, err := decodeKey(bridgeSquadsProgram)
	if err != nil {
		return nil, fmt.Errorf("decode squads program: %w", err)
	}
	addresses := make([]string, 0, len(legacyCustomPolicySeeds))
	for _, seed := range legacyCustomPolicySeeds {
		var seedLe [8]byte
		binary.LittleEndian.PutUint64(seedLe[:], seed)
		derived, err := findProgramDerivedAddress([]byte("smart_account"), program[:],
			[]byte("policy"), settings[:], seedLe[:])
		if err != nil {
			return nil, fmt.Errorf("derive seed %d policy: %w", seed, err)
		}
		addresses = append(addresses, derived)
	}
	return addresses, nil
}

// AssertLegacyPoliciesRetired reports every legacy policy address that still
// exists at finalized commitment. The Settings anchor is mandatory in the same
// getMultipleAccounts response as the optional legacy policies, and the
// genesis hash is checked before any null policy values are trusted. A
// non-empty survivor list, an invalid anchor, a wrong cluster, or any read
// failure is a startup refusal — the gate fails closed, and the supervisor
// starts the worker again.
func AssertLegacyPoliciesRetired(ctx context.Context, client *chain.Client) ([]string, error) {
	gateCtx, cancel := context.WithTimeout(ctx, legacyPolicyGateTimeout)
	defer cancel()
	legacyAddresses, err := legacyCustomPolicyAddresses()
	if err != nil {
		return nil, err
	}
	addresses := make([]string, 0, len(legacyAddresses)+1)
	addresses = append(addresses, bridgeSettings)
	addresses = append(addresses, legacyAddresses...)
	genesisHash, err := client.GenesisHash(gateCtx)
	if err != nil {
		return nil, fmt.Errorf("legacy policy gate genesis: %w", err)
	}
	if genesisHash.String() != mainnetGenesisHash {
		return nil, fmt.Errorf("legacy policy gate refuses non-mainnet genesis %q", genesisHash)
	}
	// The batch is pinned to a finalized slot.
	slot, err := finalizedSlot(gateCtx, client)
	if err != nil {
		return nil, fmt.Errorf("legacy policy gate finalized slot: %w", err)
	}
	_, accounts, err := finalizedAccounts(gateCtx, client, addresses, slot, legacyAddresses...)
	if err != nil {
		return nil, fmt.Errorf("legacy policy gate read at finalized: %w", err)
	}
	anchor := accounts[0]
	if anchor.Address != bridgeSettings || anchor.Owner != bridgeSquadsProgram || anchor.Lamports == 0 ||
		len(anchor.Data) <= len(squadsSettingsDiscriminator) ||
		!bytes.Equal(anchor.Data[:len(squadsSettingsDiscriminator)], squadsSettingsDiscriminator[:]) {
		return nil, fmt.Errorf("legacy policy gate Settings anchor is absent or invalid")
	}
	var surviving []string
	for _, account := range accounts[1:] {
		if account.Lamports > 0 || account.Owner != "" {
			surviving = append(surviving, account.Address)
		}
	}
	if len(surviving) > 0 {
		return surviving, fmt.Errorf(
			"legacy custom policies still installed at seeds 62-65; run the PolicyRemove step before starting this worker: %v",
			surviving)
	}
	return nil, nil
}

var (
	ed25519Prime, _  = new(big.Int).SetString("7fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffed", 16)
	ed25519CurveD, _ = new(big.Int).SetString(
		"37095705934669439343138083508754565189542113879843219016388785533085940283555", 10)
	ed25519SqrtM1, _ = new(big.Int).SetString(
		"19681161376707505956807079304988542015446066515923890162744021073123829784752", 10)
)

// findProgramDerivedAddress walks bumps 255..0 for the Solana PDA hash
// sha256(seeds... | bump | program | "ProgramDerivedAddress") and returns the
// base58 address of the first hash off the ed25519 curve.
func findProgramDerivedAddress(seed []byte, program []byte, extra ...[]byte) (string, error) {
	for bump := 255; bump >= 0; bump-- {
		digest := sha256.New()
		digest.Write(seed)
		for _, tail := range extra {
			digest.Write(tail)
		}
		digest.Write([]byte{byte(bump)})
		digest.Write(program)
		digest.Write([]byte("ProgramDerivedAddress"))
		sum := digest.Sum(nil)
		if !isOnCurve(sum) {
			return encodeBase58(sum), nil
		}
	}
	return "", fmt.Errorf("no bump off the ed25519 curve")
}

// isOnCurve decodes a compressed ed25519 point (RFC 8032 section 5.1.3) and
// reports whether it lands on the curve; Solana only accepts PDA hashes that
// do not.
func isOnCurve(compressed []byte) bool {
	yBytes := make([]byte, 32)
	copy(yBytes, compressed)
	yBytes[31] &= 0x7f
	y := littleEndian(yBytes)
	if y.Cmp(ed25519Prime) >= 0 {
		return false
	}
	ySquared := new(big.Int).Mul(y, y)
	ySquared.Mod(ySquared, ed25519Prime)
	numerator := new(big.Int).Sub(ySquared, big.NewInt(1))
	denominator := new(big.Int).Mul(ed25519CurveD, ySquared)
	denominator.Add(denominator, big.NewInt(1))
	denominator.ModInverse(denominator, ed25519Prime)
	xSquared := new(big.Int).Mul(numerator, denominator)
	xSquared.Mod(xSquared, ed25519Prime)
	exponent := new(big.Int).Rsh(new(big.Int).Add(ed25519Prime, big.NewInt(3)), 3)
	x := new(big.Int).Exp(xSquared, exponent, ed25519Prime)
	if !squareEquals(x, xSquared) {
		x.Mul(x, ed25519SqrtM1)
		x.Mod(x, ed25519Prime)
		if !squareEquals(x, xSquared) {
			return false
		}
	}
	return true // sign only selects which root; existence is what PDA derivation needs
}

func squareEquals(x, xSquared *big.Int) bool {
	check := new(big.Int).Mul(x, x)
	check.Mod(check, ed25519Prime)
	return check.Cmp(xSquared) == 0
}

func littleEndian(bytes []byte) *big.Int {
	reversed := make([]byte, len(bytes))
	for index, value := range bytes {
		reversed[len(bytes)-1-index] = value
	}
	return new(big.Int).SetBytes(reversed)
}
