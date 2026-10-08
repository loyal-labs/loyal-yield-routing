package backyard

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const setupKeypairEnvironment = "SOLANA_TESTING_PK"

// Credentials is the explicit Backyard signing capability: the delegated
// executor keypair for the fixed manifest lane. A loyal-engine instance owns
// one Credentials value; observers, planners, and callers of decision or
// recovery paths never receive one.
type Credentials struct {
	PolicyKey ed25519.PrivateKey
}

// signer validates the capability against the pinned delegated executor, so a
// mismatched key fails at startup instead of at first build.
func (c Credentials) signer() (ed25519.PrivateKey, error) {
	if len(c.PolicyKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("Backyard signing capability is not configured")
	}
	key := ed25519.NewKeyFromSeed(c.PolicyKey.Seed())
	if !key.Public().(ed25519.PublicKey).Equal(c.PolicyKey.Public()) {
		return nil, fmt.Errorf("Backyard signing capability public half does not match seed")
	}
	if publicKeyFromBytes(key.Public().(ed25519.PublicKey)) != mustKey(bridgeDelegate) {
		return nil, fmt.Errorf("Backyard signing capability does not match the pinned delegated executor")
	}
	return key, nil
}

// ParseCredentials follows loyal-solana-env's established input contract: a
// JSON byte array, hexadecimal bytes, or base58 bytes representing a 32-byte
// seed or 64-byte Solana secret key. Errors deliberately omit all secret data.
func ParseCredentials(material string) (Credentials, error) {
	key, err := decodeSolanaKeypairMaterial(material)
	if err != nil {
		return Credentials{}, fmt.Errorf("invalid Backyard signing configuration")
	}
	key, err = (Credentials{PolicyKey: key}).signer()
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{PolicyKey: key}, nil
}

// Setup uses the already-established Backyard Settings admin, never the
// lifecycle delegate and never a fallback key. Loading it does not enable send.
func loadPinnedPolicySetupSigner() (ed25519.PrivateKey, error) {
	return loadPinnedSigner(setupKeypairEnvironment, mustKey(bridgeSettingsSigner), "Settings admin")
}

func loadPinnedSigner(environment string, expected publicKey, role string) (ed25519.PrivateKey, error) {
	value, ok := os.LookupEnv(environment)
	if !ok || strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("%s is not configured", environment)
	}
	key, err := decodeSolanaKeypairMaterial(value)
	if err != nil {
		return nil, fmt.Errorf("%s is not a valid Solana keypair", environment)
	}
	if publicKeyFromBytes(key.Public().(ed25519.PublicKey)) != expected {
		return nil, fmt.Errorf("%s does not match the pinned %s", environment, role)
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
