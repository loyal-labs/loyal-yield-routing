package backyardrwa

import (
	"encoding/binary"
	"fmt"
)

// BasicPolicyFamily identifies one of the four shared ProgramInteraction
// policies installed by the Rust/TypeScript track. The family owns the policy
// account; each lifecycle leg selects a constraint inside that account.
type BasicPolicyFamily string

const (
	BasicCollateralLifecycle BasicPolicyFamily = "CollateralLifecycle"
	BasicDebtLifecycle       BasicPolicyFamily = "DebtLifecycle"
	BasicSwapRoutesA         BasicPolicyFamily = "SwapRoutesA"
	BasicSwapRoutesB         BasicPolicyFamily = "SwapRoutesB"
)

type BasicPolicyBinding struct {
	Family BasicPolicyFamily
	Seed   uint64
	Policy string
	Index  map[string]byte
}

var basicPolicySeeds = map[BasicPolicyFamily]uint64{
	BasicCollateralLifecycle: 141,
	BasicDebtLifecycle:       142,
	BasicSwapRoutesA:         143,
	BasicSwapRoutesB:         144,
}

// derivePolicyAccount derives the physical Squads policy PDA from the same
// settings account and seed counter used by the Rust installer.
func derivePolicyAccount(seed uint64) (string, error) {
	settings, err := decodeKey(bridgeSettings)
	if err != nil {
		return "", fmt.Errorf("decode settings: %w", err)
	}
	program, err := decodeKey(bridgeSquadsProgram)
	if err != nil {
		return "", fmt.Errorf("decode Squads program: %w", err)
	}
	var seedLE [8]byte
	binary.LittleEndian.PutUint64(seedLE[:], seed)
	return findProgramDerivedAddress(
		[]byte("smart_account"), program[:], []byte("policy"), settings[:], seedLE[:],
	)
}

func basicPolicyBinding(family BasicPolicyFamily) (BasicPolicyBinding, error) {
	seed, ok := basicPolicySeeds[family]
	if !ok {
		return BasicPolicyBinding{}, fmt.Errorf("unknown basic policy family %q", family)
	}
	policy, err := derivePolicyAccount(seed)
	if err != nil {
		return BasicPolicyBinding{}, err
	}
	indices := map[string]byte{}
	switch family {
	case BasicCollateralLifecycle:
		indices["deposit"] = 0
		indices["withdraw"] = 1
	case BasicDebtLifecycle:
		indices["borrow"] = 0
		indices["repay"] = 1
	case BasicSwapRoutesA, BasicSwapRoutesB:
		indices["route0"] = 0
		indices["route1"] = 1
	}
	return BasicPolicyBinding{Family: family, Seed: seed, Policy: policy, Index: indices}, nil
}

func basicPolicySet() (map[BasicPolicyFamily]BasicPolicyBinding, error) {
	set := make(map[BasicPolicyFamily]BasicPolicyBinding, len(basicPolicySeeds))
	for _, family := range []BasicPolicyFamily{
		BasicCollateralLifecycle, BasicDebtLifecycle, BasicSwapRoutesA, BasicSwapRoutesB,
	} {
		binding, err := basicPolicyBinding(family)
		if err != nil {
			return nil, err
		}
		set[family] = binding
	}
	return set, nil
}

func basicPolicyConstraintIndex(leg kaminoPrimeUSDCLeg) byte {
	switch leg {
	case kaminoLegDeposit:
		return 0
	case kaminoLegWithdraw:
		return 1
	case kaminoLegBorrow:
		return 0
	case kaminoLegRepay:
		return 1
	default:
		return 0xff
	}
}

func basicPolicyFamilyForKaminoLeg(leg kaminoPrimeUSDCLeg) BasicPolicyFamily {
	switch leg {
	case kaminoLegDeposit, kaminoLegWithdraw:
		return BasicCollateralLifecycle
	case kaminoLegBorrow, kaminoLegRepay:
		return BasicDebtLifecycle
	default:
		return ""
	}
}

// resolveBasicSwapPolicy is deliberately an exact custody-pair allowlist.
// The policy constraints are bicliques, but the worker still resolves the
// selected route by its concrete custody pair before constructing a packet.
func resolveBasicSwapPolicy(sourceCustody, destinationCustody string) (BasicPolicyBinding, byte, error) {
	set, err := basicPolicySet()
	if err != nil {
		return BasicPolicyBinding{}, 0, err
	}
	pairs := []struct {
		source, destination string
		family              BasicPolicyFamily
		index               byte
	}{
		{bridgeSquadsATA, kaminoPrimeCustody, BasicSwapRoutesA, 0},
		{bridgeSquadsATA, mapleSyrupUSDCUSDC.CollateralCustody, BasicSwapRoutesA, 1},
		{bridgeSquadsATA, "AVX9wxDTk639eZ4KaiMA7LrLhXe7Lg6DaDDVRa1Q7Ji3", BasicSwapRoutesA, 0},
		{kaminoPrimeCustody, bridgeSquadsATA, BasicSwapRoutesB, 0},
		{mapleSyrupUSDCUSDC.CollateralCustody, bridgeSquadsATA, BasicSwapRoutesB, 1},
		{"AVX9wxDTk639eZ4KaiMA7LrLhXe7Lg6DaDDVRa1Q7Ji3", bridgeSquadsATA, BasicSwapRoutesB, 0},
	}
	for _, pair := range pairs {
		if sourceCustody == pair.source && destinationCustody == pair.destination {
			return set[pair.family], pair.index, nil
		}
	}
	return BasicPolicyBinding{}, 0, fmt.Errorf("custody pair %s -> %s is not installed in the basic swap policy set", sourceCustody, destinationCustody)
}

func deriveKaminoObligationFarmUserState(reserveFarmState, obligation string) (string, error) {
	farm, err := decodeKey(reserveFarmState)
	if err != nil {
		return "", fmt.Errorf("decode reserve farm state: %w", err)
	}
	obligationKey, err := decodeKey(obligation)
	if err != nil {
		return "", fmt.Errorf("decode obligation: %w", err)
	}
	farmsProgram, err := decodeKey("FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr")
	if err != nil {
		return "", fmt.Errorf("decode Farms program: %w", err)
	}
	return findProgramDerivedAddress([]byte("user"), farmsProgram[:], farm[:], obligationKey[:])
}

const onreDebtFarmState = "7vNfe1qX8iDxP5p3A4fosrjLqdn1YjmmGcZZkG2b4APF"

func onreDebtFarmPair() (reserveFarmState, obligationFarmUserState string, err error) {
	reserveFarmState = onreDebtFarmState
	obligationFarmUserState, err = deriveKaminoObligationFarmUserState(
		reserveFarmState, "4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei",
	)
	return
}
