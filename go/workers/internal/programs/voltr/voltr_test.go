package voltr

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
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

func TestManagementFeeDilutesLPValueLikeWeb(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	data := make([]byte, VaultSize)
	copy(data, VaultDiscriminator[:])
	binary.LittleEndian.PutUint64(data[168:], 1_000_000)
	binary.LittleEndian.PutUint16(data[516:], 1_000)
	binary.LittleEndian.PutUint64(data[568:], uint64(now.Unix()-365*24*60*60))
	vault, err := DecodeVault(&chain.Account{Owner: ProgramID, Lamports: 1, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	got, err := vault.LPToAssets(1_000_000, 1_000_000, now)
	if err != nil || got != 899_999 {
		t.Fatalf("value=%d err=%v, want 899999", got, err)
	}
}
