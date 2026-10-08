package fleetexec

import (
	"fmt"
	"reflect"
	"testing"
)

func TestLookupPackingPreservesWholeManifestReservationAndDeterminism(t *testing.T) {
	policy := LookupPackingPolicy{HardCapacity: 256, LargestAtomicExpansion: 20, SafetyMargin: 8, GrowthReservation: 8, MaximumVaultCohort: 16}
	demand := LookupVaultDemand{Addresses: []string{"c", "a", "b"}, NextGeneration: 2, NextShardOrdinal: 9}
	candidates := []LookupShardCandidate{
		{TableID: 10, FamilyID: 1, Generation: 1, ShardOrdinal: 1, Confirmed: []string{"a", "b"}, Pending: []string{"c"}, ReservedCount: 220, HighWater: 228, BoundVaults: 1, Accepting: true, Lifecycle: "active"},
		{TableID: 11, FamilyID: 1, Generation: 1, ShardOrdinal: 2, Confirmed: []string{"a", "b"}, ReservedCount: 10, HighWater: 228, BoundVaults: 1, Accepting: true, Lifecycle: "active"},
		{TableID: 12, FamilyID: 1, Generation: 1, ShardOrdinal: 3, Confirmed: []string{"a", "b", "c"}, ReservedCount: 200, HighWater: 228, BoundVaults: 1, Accepting: true, Lifecycle: "active"},
	}
	allocation, err := allocateLookupVault(demand, candidates, policy)
	if err != nil || allocation.TableID != 12 || allocation.ReservedCapacity != 11 || allocation.ProjectedCommitment != 211 || len(allocation.Missing) != 0 {
		t.Fatalf("unsafe allocation %+v %v", allocation, err)
	}
	for i, j := 0, len(candidates)-1; i < j; i, j = i+1, j-1 {
		candidates[i], candidates[j] = candidates[j], candidates[i]
	}
	permuted, err := allocateLookupVault(demand, candidates, policy)
	if err != nil || !reflect.DeepEqual(permuted, allocation) {
		t.Fatal("input order changed source best-fit result", permuted, err)
	}
	demand.CurrentTableID = 12
	demand.CurrentReservedCapacity = 11
	candidates[0].Lifecycle = "standby"
	candidates[0].Accepting = false
	kept, err := allocateLookupVault(demand, candidates, policy)
	if err != nil || !kept.Keep || kept.TableID != 12 {
		t.Fatal("verified current standby binding moved unnecessarily", kept, err)
	}
	demand.Addresses = nil
	for n := 0; n < 230; n++ {
		demand.Addresses = append(demand.Addresses, fmt.Sprintf("address-%03d", n))
	}
	dedicated, err := allocateLookupVault(demand, nil, policy)
	if err != nil || !dedicated.Dedicated || dedicated.HighWater != 256 || dedicated.ReservedCapacity != 238 {
		t.Fatal("large cohort not isolated as dedicated", dedicated, err)
	}
	demand.Addresses = append(demand.Addresses, demand.Addresses[:20]...)
	// Repeated demand is a set, never double reserved.
	if same, err := allocateLookupVault(demand, nil, policy); err != nil || same.ReservedCapacity != 238 {
		t.Fatal("duplicate demand consumed capacity", same, err)
	}
	for n := 230; n < 249; n++ {
		demand.Addresses = append(demand.Addresses, fmt.Sprintf("address-%03d", n))
	}
	if _, err := allocateLookupVault(demand, nil, policy); err == nil {
		t.Fatal("whole-vault plus growth silently truncated beyond 256")
	}
}

func TestLookupCatalogKeepsAppendPrefixesAndRefusesConcurrentSuffix(t *testing.T) {
	initial, err := lookupCatalogShards([]string{"a", "b", "c", "d", "e"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	appended, err := lookupCatalogShards([]string{"a", "b", "c", "d", "e", "f", "g"}, 2)
	if err != nil || !reflect.DeepEqual(initial[:2], appended[:2]) {
		t.Fatal("catalog append relocated full shards", appended, err)
	}
	kind, suffix, err := nextLookupCatalogMutation(true, appended[2], []string{"e"}, nil, 20)
	if err != nil || kind != LookupExtend || !reflect.DeepEqual(suffix, []string{"f"}) {
		t.Fatal("wrong ordered extension", kind, suffix, err)
	}
	if kind, suffix, err = nextLookupCatalogMutation(true, appended[2], []string{"e"}, []string{"f"}, 20); err != nil || kind != "" || len(suffix) != 0 {
		t.Fatal("in-flight suffix cloned", kind, suffix, err)
	}
	if _, _, err = nextLookupCatalogMutation(true, []string{"f", "e"}, []string{"e"}, nil, 20); err == nil {
		t.Fatal("prefix drift mutated in-place instead of rollover")
	}
}
