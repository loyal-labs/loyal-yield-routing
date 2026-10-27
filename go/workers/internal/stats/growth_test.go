package stats

import (
	"math/big"
	"testing"
	"time"

	solana "github.com/solana-foundation/solana-go/v2"
)

func TestCombineGrowthCountsEachUserOnceAcrossEarnAndEarnMax(t *testing.T) {
	settings := solana.NewWallet().PublicKey()
	vault, _, err := solana.FindProgramAddress([][]byte{[]byte("smart_account"), settings[:], []byte("smart_account"), {0}}, solana.MustPublicKeyFromBase58(smartAccountsID))
	if err != nil {
		t.Fatal(err)
	}
	small := solana.NewWallet().PublicKey().String()
	outsider := solana.NewWallet().PublicKey().String()

	got := combineGrowth(
		map[string]*big.Int{
			settings.String(): big.NewInt(600_000), // $0.60 in Earn
			small:             big.NewInt(1_000_000),
		},
		map[string]*big.Int{
			vault.String(): big.NewInt(600_000),         // + $0.60 in Earn MAX: same user, $1.20 combined
			outsider:       big.NewInt(201_000_000_000), // Earn MAX holder that is not a Loyal smart account
		},
		time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
	)

	// settings ($1.20 combined) and outsider count; small sits exactly at $1, not above it.
	if got.ActiveUsers != 2 {
		t.Fatalf("active users = %d, want 2", got.ActiveUsers)
	}
	if got.EarnUSD != 1.6 || got.EarnMaxUSD != 201_000.6 || got.TVLUSD != 201_002.2 {
		t.Fatalf("usd = earn %v, earn max %v, tvl %v", got.EarnUSD, got.EarnMaxUSD, got.TVLUSD)
	}
	if got.ObservedAt != "2026-10-27T12:00:00.000Z" {
		t.Fatalf("observedAt = %s", got.ObservedAt)
	}
}
