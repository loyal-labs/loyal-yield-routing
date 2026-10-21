package backyard

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

type Credentials struct {
	key ed25519.PrivateKey
}

func (c Credentials) signer() (ed25519.PrivateKey, error) {
	if len(c.key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("Backyard signing capability is not configured")
	}
	key := ed25519.NewKeyFromSeed(c.key.Seed())
	if !key.Public().(ed25519.PublicKey).Equal(c.key.Public()) {
		return nil, fmt.Errorf("Backyard signing capability public half does not match seed")
	}
	if publicKeyFromBytes(key.Public().(ed25519.PublicKey)) != mustKey(bridgeDelegate) {
		return nil, fmt.Errorf("Backyard signing capability does not match the pinned delegated executor")
	}
	return key, nil
}

// ParseCredentials accepts a JSON byte array, hexadecimal bytes, or base58
// bytes representing a 32-byte seed or 64-byte Solana secret key. Errors omit
// all secret data.
func ParseCredentials(material string) (Credentials, error) {
	key, err := decodeSolanaKeypairMaterial(material)
	if err != nil {
		return Credentials{}, fmt.Errorf("invalid Backyard signing configuration")
	}
	key, err = (Credentials{key: key}).signer()
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{key: key}, nil
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
