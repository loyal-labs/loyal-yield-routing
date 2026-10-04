package fleetexec

import (
	"errors"
	"slices"
)

// These planner values mirror the source packed allocator. They contain only
// catalog/membership/capacity, with no RPC send or signing capability.
type LookupPackingPolicy struct {
	HardCapacity, LargestAtomicExpansion, SafetyMargin int
	GrowthReservation, MaximumVaultCohort              int
}
type LookupShardCandidate struct {
	TableID, FamilyID                     int64
	Generation, ShardOrdinal              int32
	Confirmed, Pending                    []string
	ReservedCount, HighWater, BoundVaults int
	Accepting                             bool
	Lifecycle                             string
}
type LookupVaultDemand struct {
	Addresses                        []string
	CurrentTableID                   int64
	CurrentReservedCapacity          int
	NextGeneration, NextShardOrdinal int32
}
type LookupAllocation struct {
	Keep                                                                                  bool
	TableID, FamilyID                                                                     int64
	Generation, ShardOrdinal                                                              int32
	Missing                                                                               []string
	ReservedCapacity, ReservationDelta, ProjectedOccupied, ProjectedCommitment, HighWater int
	Dedicated                                                                             bool
}

func lookupAddressSet(addresses []string) map[string]bool {
	set := make(map[string]bool, len(addresses))
	for _, address := range addresses {
		set[address] = true
	}
	return set
}
func lookupSortedSet(set map[string]bool) []string {
	result := make([]string, 0, len(set))
	for address := range set {
		result = append(result, address)
	}
	slices.Sort(result)
	return result
}

func allocateLookupVault(demand LookupVaultDemand, candidates []LookupShardCandidate, policy LookupPackingPolicy) (LookupAllocation, error) {
	if policy.HardCapacity <= 0 || policy.HardCapacity > 256 || policy.LargestAtomicExpansion <= 0 || policy.SafetyMargin <= 0 || policy.LargestAtomicExpansion+policy.SafetyMargin >= policy.HardCapacity || policy.GrowthReservation < 0 || policy.GrowthReservation > 256 || policy.MaximumVaultCohort <= 0 || policy.MaximumVaultCohort > 65535 || demand.CurrentReservedCapacity < 0 || demand.NextGeneration < 0 || demand.NextShardOrdinal < 0 {
		return LookupAllocation{}, errors.New("lookup packed allocation policy invalid")
	}
	desired := lookupAddressSet(demand.Addresses)
	if desired[""] {
		return LookupAllocation{}, errors.New("lookup vault demand contains empty address")
	}
	reserved := len(desired) + policy.GrowthReservation
	if reserved > policy.HardCapacity {
		return LookupAllocation{}, errors.New("lookup whole-vault manifest plus growth exceeds physical capacity")
	}
	highWater := policy.HardCapacity - policy.LargestAtomicExpansion - policy.SafetyMargin
	for _, candidate := range candidates {
		if candidate.TableID == demand.CurrentTableID && (candidate.Lifecycle == "active" || candidate.Lifecycle == "standby") && demand.CurrentReservedCapacity >= reserved {
			confirmed := lookupAddressSet(candidate.Confirmed)
			complete := true
			for address := range desired {
				complete = complete && confirmed[address]
			}
			if complete {
				return LookupAllocation{Keep: true, TableID: candidate.TableID, FamilyID: candidate.FamilyID}, nil
			}
		}
	}
	type scored struct {
		allocation                LookupAllocation
		missing, residual, cohort int
	}
	var best *scored
	for _, candidate := range candidates {
		if !candidate.Accepting || (candidate.Lifecycle != "preparing" && candidate.Lifecycle != "warming" && candidate.Lifecycle != "active") || candidate.ReservedCount < 0 || candidate.ReservedCount > 256 || candidate.HighWater <= 0 || candidate.HighWater > 256 || candidate.BoundVaults < 0 || candidate.BoundVaults > 65535 || candidate.Generation < 0 || candidate.ShardOrdinal < 0 || candidate.TableID <= 0 || candidate.FamilyID <= 0 {
			continue
		}
		already := candidate.TableID == demand.CurrentTableID
		cohort := candidate.BoundVaults
		if !already {
			cohort++
		}
		if cohort > policy.MaximumVaultCohort {
			continue
		}
		occupied := lookupAddressSet(append(append([]string{}, candidate.Confirmed...), candidate.Pending...))
		missing := make(map[string]bool)
		for address := range desired {
			if !occupied[address] {
				missing[address] = true
			}
		}
		projected := len(occupied) + len(missing)
		prior := 0
		if already {
			prior = demand.CurrentReservedCapacity
		}
		delta := max(0, reserved-prior)
		commitment := max(projected, candidate.ReservedCount+delta)
		water := min(highWater, candidate.HighWater)
		if projected > policy.HardCapacity || commitment > water {
			continue
		}
		current := scored{LookupAllocation{TableID: candidate.TableID, FamilyID: candidate.FamilyID, Missing: lookupSortedSet(missing), ReservedCapacity: reserved, ReservationDelta: delta, ProjectedOccupied: projected, ProjectedCommitment: commitment, HighWater: water}, len(missing), water - commitment, cohort}
		less := best == nil
		if best != nil {
			left := []int64{int64(current.missing), int64(current.residual), int64(current.cohort), int64(candidate.Generation), int64(candidate.ShardOrdinal), candidate.TableID}
			right := []int64{int64(best.missing), int64(best.residual), int64(best.cohort), int64(best.allocation.Generation), int64(best.allocation.ShardOrdinal), best.allocation.TableID}
			less = slices.Compare(left, right) < 0
		}
		current.allocation.Generation = candidate.Generation
		current.allocation.ShardOrdinal = candidate.ShardOrdinal
		if less {
			best = &current
		}
	}
	if best != nil {
		return best.allocation, nil
	}
	dedicated := reserved > highWater
	if dedicated {
		highWater = policy.HardCapacity
	}
	return LookupAllocation{Generation: demand.NextGeneration, ShardOrdinal: demand.NextShardOrdinal, Missing: lookupSortedSet(desired), ReservedCapacity: reserved, ProjectedOccupied: len(desired), ProjectedCommitment: reserved, HighWater: highWater, Dedicated: dedicated}, nil
}

// Catalog publication is append-packed in persisted ordinal order. Reordering
// its confirmed prefix requires a later generation, never an in-place extension.
func lookupCatalogShards(ordered []string, capacity int) ([][]string, error) {
	if capacity <= 0 || capacity > 256 || len(lookupAddressSet(ordered)) != len(ordered) {
		return nil, errors.New("lookup catalog capacity or ordered uniqueness invalid")
	}
	var shards [][]string
	for offset := 0; offset < len(ordered); offset += capacity {
		shards = append(shards, append([]string{}, ordered[offset:min(offset+capacity, len(ordered))]...))
	}
	return shards, nil
}
func nextLookupCatalogMutation(exists bool, desired, confirmed, pending []string, chunk int) (LookupKind, []string, error) {
	if chunk <= 0 || chunk > lookupMaximumChunk {
		return "", nil, errors.New("lookup catalog chunk invalid")
	}
	if len(confirmed) > len(desired) || !lookupSameAddresses(confirmed, desired[:min(len(confirmed), len(desired))]) {
		return "", nil, errors.New("lookup catalog requires generation rollover for prefix drift")
	}
	if len(pending) != 0 || len(confirmed) == len(desired) {
		return "", nil, nil
	}
	kind := LookupCreate
	if exists {
		kind = LookupExtend
	}
	return kind, append([]string{}, desired[len(confirmed):min(len(confirmed)+chunk, len(desired))]...), nil
}
