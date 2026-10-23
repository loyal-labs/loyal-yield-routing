package earnprep

import (
	"errors"
	"strings"

	"github.com/solana-foundation/solana-go/v2"
)

func ParsePolicySigner(raw string) (solana.PublicKey, error) {
	key, err := solana.PublicKeyFromBase58(strings.TrimSpace(raw))
	if err != nil {
		return solana.PublicKey{}, errors.New("EARN_YIELD_ROUTER_PUBLIC_KEY: want a base58 public key")
	}
	return key, nil
}
