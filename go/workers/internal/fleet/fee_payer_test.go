package fleet

import (
	"crypto/sha256"
	"testing"
)

func feePayerKey(seed byte) string {
	var key [32]byte
	for i := range key {
		key[i] = seed + byte(i)
	}
	return encodeBase58(key[:])
}

func TestRouteFeePayerSpreadsVaultsOverAFixedListDeterministically(t *testing.T) {
	// Root cause of the Rust shards: every route write-locks its fee payer, so
	// one shared payer serializes the fleet within a block.
	policy := feePayerKey(1)
	payers := []string{feePayerKey(10), feePayerKey(20), feePayerKey(30)}
	if got := RouteFeePayer("mainnet-beta", feePayerKey(99), policy, nil); got != policy {
		t.Fatalf("no fee-only list must keep the policy payer, got %s", got)
	}
	used := map[string]int{}
	for seed := byte(100); seed < 160; seed++ {
		vault := feePayerKey(seed)
		got := RouteFeePayer("mainnet-beta", vault, policy, payers)
		reversed := RouteFeePayer("mainnet-beta", vault, policy, []string{payers[2], payers[1], payers[0]})
		if got != reversed || got == policy {
			t.Fatalf("payer for %s depends on list order or fell back: %s %s", vault, got, reversed)
		}
		used[got]++
	}
	if len(used) != len(payers) {
		t.Fatalf("vaults did not spread over every payer: %v", used)
	}
}

func TestRouteFeePayerMatchesRustRendezvousScore(t *testing.T) {
	// fee_payer_rendezvous_score hashes cluster, 0, vault, 0 and the raw
	// 32-byte payer; the highest score wins.
	vault := feePayerKey(77)
	payers := []string{feePayerKey(10), feePayerKey(20)}
	score := func(payer string) [32]byte {
		key, _ := decodePublicKey(payer)
		return sha256.Sum256(append(append(append([]byte("mainnet-beta\x00"), vault...), 0), key[:]...))
	}
	a, b := score(payers[0]), score(payers[1])
	want := payers[0]
	if string(b[:]) > string(a[:]) {
		want = payers[1]
	}
	if got := RouteFeePayer("mainnet-beta", vault, feePayerKey(1), payers); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}
