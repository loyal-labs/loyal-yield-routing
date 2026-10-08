package solana

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"testing"
)

func TestCollateralConversionUsesExchangeUnitsAndFloors(t *testing.T) {
	got, err := RedeemableLiquidity(3, 5, 2)
	if err != nil || got != 7 {
		t.Fatalf("converted %d: %v", got, err)
	}
	if _, err = RedeemableLiquidity(1, 1, 0); err == nil {
		t.Fatal("missing exchange accepted")
	}
	if _, err = RedeemableLiquidity(CollateralRaw(math.MaxUint64), 2, 1); !errors.Is(err, ErrAmountRange) {
		t.Fatal("overflow accepted")
	}
	if _, err = SQLAmount(math.MaxUint64); !errors.Is(err, ErrAmountRange) {
		t.Fatal("chain amount overflowed SQL range")
	}
}

func TestAttemptOwnsImmutableWire(t *testing.T) {
	source := []byte{1, 2, 3}
	sum := sha256.Sum256(source)
	hash := hex.EncodeToString(sum[:])
	wire, err := OwnSignedWire(source, hash)
	if err != nil {
		t.Fatal(err)
	}
	source[0] = 9
	exported := wire.Bytes()
	exported[0] = 8
	if wire.Bytes()[0] != 1 || wire.Hash() != hash {
		t.Fatal("caller mutated owned attempt")
	}
	if _, err := OwnSignedWire([]byte{1, 2, 4}, hash); err == nil {
		t.Fatal("persisted hash discrepancy accepted")
	}
}
