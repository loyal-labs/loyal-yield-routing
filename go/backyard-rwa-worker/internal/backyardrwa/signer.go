package backyardrwa

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const policyKeypairEnvironment = "POLICY_KEYPAIR"

// loadPinnedPolicySigner follows loyal-solana-env's established input contract:
// a JSON byte array, hexadecimal bytes, or base58 bytes representing a 32-byte
// seed or 64-byte Solana secret key. Errors deliberately omit all secret data.
func loadPinnedPolicySigner() (ed25519.PrivateKey, error) {
	value, ok := os.LookupEnv(policyKeypairEnvironment)
	if !ok || strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("%s is not configured", policyKeypairEnvironment)
	}
	key, err := decodeSolanaKeypairMaterial(value)
	if err != nil {
		return nil, fmt.Errorf("%s is not a valid Solana keypair", policyKeypairEnvironment)
	}
	if publicKeyFromBytes(key.Public().(ed25519.PublicKey)) != mustKey(bridgeDelegate) {
		return nil, fmt.Errorf("%s does not match the pinned delegated executor", policyKeypairEnvironment)
	}
	return key, nil
}

func decodeSolanaKeypairMaterial(value string) (ed25519.PrivateKey, error) {
	value = strings.TrimSpace(value)
	var raw []byte
	var err error
	switch {
	case strings.HasPrefix(value, "["):
		var bytes []uint8
		if err = json.Unmarshal([]byte(value), &bytes); err != nil {
			return nil, err
		}
		raw = bytes
	case isHexKeyMaterial(value):
		value = strings.TrimPrefix(strings.TrimPrefix(value, "0x"), "0X")
		raw, err = hex.DecodeString(value)
	default:
		raw, err = decodeBase58(value)
	}
	if err != nil {
		return nil, err
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize:
		key := ed25519.PrivateKey(append([]byte(nil), raw...))
		derived := ed25519.NewKeyFromSeed(key.Seed())
		if !derived.Public().(ed25519.PublicKey).Equal(key.Public()) {
			return nil, fmt.Errorf("secret key public half does not match seed")
		}
		return key, nil
	default:
		return nil, fmt.Errorf("invalid keypair length")
	}
}

func isHexKeyMaterial(value string) bool {
	value = strings.TrimPrefix(strings.TrimPrefix(value, "0x"), "0X")
	if value == "" || len(value)%2 != 0 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}
