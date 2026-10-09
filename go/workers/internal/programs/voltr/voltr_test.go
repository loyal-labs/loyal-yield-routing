package voltr

import (
	"testing"

	"github.com/solana-foundation/solana-go/v2"
)

// The receipt PDA seeds must match @voltr/vault-sdk's vector, or every
// pending withdrawal fails the canonical-address check.
func TestWithdrawalReceiptAddressMatchesSDKVector(t *testing.T) {
	address, bump, err := WithdrawalReceiptAddress(
		solana.MustPublicKeyFromBase58("9pnHBxUqgspqQjeVtFj9qHPGPGXRvc1qC5SDjMChSuuW"),
		solana.MustPublicKeyFromBase58("4vJ9JU1bJJ34wKnjrFqrGd5bdDhxFqSMozMDeM4V5UuQ"),
	)
	if err != nil || address.String() != "BbpPz4dapzgmaZ28jwZRYwF4ZePgj7wXqK6FDaDDYpEz" || bump != 255 {
		t.Fatalf("PDA=%s bump=%d err=%v", address, bump, err)
	}
}
