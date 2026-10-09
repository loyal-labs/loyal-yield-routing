package chain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// SignedWire owns exact, previously verified transaction bytes. This container
// protects byte identity; family receipt/wire validation proves authorization.
type SignedWire struct {
	bytes []byte
	hash  [32]byte
}

func OwnSignedWire(wire []byte, expectedHash string) (SignedWire, error) {
	if len(wire) == 0 || len(wire) > 1232 {
		return SignedWire{}, errors.New("invalid transaction packet size")
	}
	owned := append([]byte(nil), wire...)
	hash := sha256.Sum256(owned)
	if expectedHash != hex.EncodeToString(hash[:]) {
		return SignedWire{}, errors.New("transaction wire hash mismatch")
	}
	return SignedWire{bytes: owned, hash: hash}, nil
}

func (w SignedWire) Bytes() []byte { return append([]byte(nil), w.bytes...) }
func (w SignedWire) Hash() string  { return hex.EncodeToString(w.hash[:]) }
