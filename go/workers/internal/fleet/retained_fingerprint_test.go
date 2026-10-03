package fleet

import (
	"math"
	"testing"
)

// Golden bytes follow Rust stable_fingerprint: each UTF-8 part is prefixed
// with its little-endian u64 byte length, not JSON or delimiter concatenation.
func TestRetainedSameMintRouteFingerprint(t *testing.T) {
	lease := RevalidationLease{Cluster: "localnet", VaultID: 1, SourceReserve: "source", TargetReserve: "target"}
	const expected = "30945e54a3efdd097b526fea0da96d7a6262538fb71a5d467475213ce01a2c5a"
	if got := retainedSameMintRouteFingerprint(lease); got != expected {
		t.Fatalf("retained identity contract: got %s want %s", got, expected)
	}
	mutations := []RevalidationLease{lease, lease, lease, lease}
	mutations[0].Cluster = "other"
	mutations[1].VaultID = 2
	mutations[2].SourceReserve = "other"
	mutations[3].TargetReserve = "other"
	for _, mutated := range mutations {
		if retainedSameMintRouteFingerprint(mutated) == expected {
			t.Fatal("changed route identity kept the same fence")
		}
	}
	swapped := lease
	swapped.SourceReserve, swapped.TargetReserve = lease.TargetReserve, lease.SourceReserve
	if retainedSameMintRouteFingerprint(swapped) == expected {
		t.Fatal("route direction must be fenced")
	}
}

// Independently calculated with Python:
// sha256(b”.join(struct.pack('<Q', len(s.encode())) + s.encode() for s in parts))
// where parts = ['same_mint_kamino', cluster, decimal_vault_id, source, target].
// These protect UTF-8 byte lengths, embedded delimiters, decimal i64 formatting,
// and field framing; the strings are wire vectors, not valid reserve addresses.
func TestRetainedSameMintRouteFingerprintWireVectors(t *testing.T) {
	for _, tt := range []struct {
		name  string
		lease RevalidationLease
		want  string
	}{
		{"unicode_and_i64", RevalidationLease{Cluster: "本地🌐", VaultID: math.MaxInt64, SourceReserve: "a\x00b", TargetReserve: "c"}, "04fecbbd2b659c0ab1d8ea01bfc969adbca2b9f1d45f65b57c47af35d7e813da"},
		{"framing_left", RevalidationLease{SourceReserve: "ab", TargetReserve: "c"}, "c66e4f4225fea9b5f7f565c67e29e4762c51f765923f0138802f69e49c115a9c"},
		{"framing_right", RevalidationLease{SourceReserve: "a", TargetReserve: "bc"}, "7185d81e8c5668277a107bcca96339bbbeaff03e113bf846de59a922ca6a0a72"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := retainedSameMintRouteFingerprint(tt.lease); got != tt.want {
				t.Fatalf("got %s want %s", got, tt.want)
			}
		})
	}
}
