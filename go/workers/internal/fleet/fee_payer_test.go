package fleet

import (
	"testing"
)

func feePayerKey(seed byte) string {
	var key [32]byte
	for i := range key {
		key[i] = seed + byte(i)
	}
	return encodeBase58(key[:])
}

func TestRankFeePayersSpreadsVaultsDeterministically(t *testing.T) {
	// Root cause of the Rust shards: every route write-locks its fee payer, so
	// one shared payer serializes the fleet within a block.
	payers := []string{feePayerKey(10), feePayerKey(20), feePayerKey(30)}
	used := map[string]int{}
	for seed := byte(100); seed < 160; seed++ {
		vault := feePayerKey(seed)
		got := RankFeePayers("mainnet-beta", vault, payers)
		reversed := RankFeePayers("mainnet-beta", vault, []string{payers[2], payers[1], payers[0]})
		if len(got) != 3 || got[0] != reversed[0] || got[1] != reversed[1] {
			t.Fatalf("order for %s depends on input order: %v %v", vault, got, reversed)
		}
		used[got[0]]++
	}
	if len(used) != len(payers) {
		t.Fatalf("vaults did not spread over every payer: %v", used)
	}
}

func TestUnhealthyShardIsSkippedNotFatal(t *testing.T) {
	// A top-ranked shard that cannot fund the route must not strand its
	// vaults: Rust moves to the next ranked shard, then to the policy signer.
	shard := FeePayerShard{MinBalance: 1_000_000, MaxBalance: 100_000_000, MaxWindowSpend: 50_000, MaxTxFee: 20_000, Reserved: 40_000}
	if shard.healthy(50_000_000, 20_000) {
		t.Fatal("exhausted rolling window was admitted")
	}
	if !shard.healthy(50_000_000, 10_000) || shard.healthy(1_005_000, 10_000) || shard.healthy(200_000_000, 10_000) {
		t.Fatal("floor/ceiling budget differs from Rust")
	}
}
