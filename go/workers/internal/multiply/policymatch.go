package multiply

// Faithful port of the Squads ProgramInteraction policy account decoder and
// the canonical Earn MAX constraint builder:
//   crates/loyal-actions/src/detection.rs
//     decode_program_interaction_policy_account and its readers,
//   crates/loyal-actions/src/earn_max.rs
//     earn_max_policy_constraints,
//   crates/loyal-actions/src/squads.rs
//     semantic_program_interaction_constraints,
//   91694cd9^:crates/loyal-fleet-worker/src/multiply/policy.rs
//     current_policy_matches.
// The comparison is structural over the decoded payload view, exactly like
// canonical_policy_payload_matches in the Rust worker.

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

// CanonicalConstraints is the Go port of earn_max_policy_constraints over the
// semantic contract: literal KLend lane pinning for collateral/debt families
// and Jupiter SharedAccountsRoute lane pinning for swaps. Constraint indexes
// line up with ConstraintIndexes in policy.go.
func CanonicalConstraints(topology *EarnMaxTopology, family PolicyFamily) (constraints []squads.InstructionConstraintView, err error) {
	// Official semantic_program_interaction_constraints sorts each instruction's
	// account clauses before serialization; compare that actual account view.
	defer func() {
		if err == nil {
			for i := range constraints {
				sort.Slice(constraints[i].AccountConstraints, func(a, b int) bool {
					return constraints[i].AccountConstraints[a].AccountIndex < constraints[i].AccountConstraints[b].AccountIndex
				})
			}
		}
	}()
	strategies := topology.StrategyCatalog()
	if len(strategies) == 0 {
		return nil, errors.New("earn max policy boundary has no lanes")
	}
	onyc, err := topology.Strategy(OnycUsdc)
	if err != nil {
		return nil, err
	}
	prime, err := topology.Strategy(PrimeUsdc)
	if err != nil {
		return nil, err
	}
	syrupUsdc, err := topology.Strategy(SyrupUsdcUsdc)
	if err != nil {
		return nil, err
	}
	usds, err := topology.Strategy(OnycUsds)
	if err != nil {
		return nil, err
	}
	pyusd, err := topology.Strategy(PrimePyusd)
	if err != nil {
		return nil, err
	}
	boundary := policyBoundary{
		vault:            topology.Vault,
		klendProgram:     mustKey(KlendProgram),
		jupiterProgram:   mustKey(JupiterProgram),
		usdcCustody:      onyc.DebtCustody,
		pyusdCustody:     pyusd.DebtCustody,
		usdsCustody:      usds.DebtCustody,
		onycCustody:      onyc.CollateralCustody,
		primeCustody:     prime.CollateralCustody,
		syrupUsdcCustody: syrupUsdc.CollateralCustody,
	}
	for _, strategy := range strategies {
		boundary.lanes = append(boundary.lanes, policyLane{
			market: strategy.Market, obligation: strategy.Obligation,
			collateralReserve: strategy.CollateralReserve, collateralCustody: strategy.CollateralCustody,
			debtReserve: strategy.DebtReserve, debtCustody: strategy.DebtCustody,
			debtTokenProgram: strategy.DebtTokenProgram,
		})
	}
	switch family {
	case FamilyCollateral:
		return []squads.InstructionConstraintView{
			collateralConstraint(boundary, DiscriminatorDepositCollateral),
			collateralConstraint(boundary, DiscriminatorWithdrawCollateral),
		}, nil
	case FamilyDebt:
		return []squads.InstructionConstraintView{
			{
				ProgramID: boundary.klendProgram,
				AccountConstraints: []squads.AccountConstraintView{
					pinned(0, boundary.vault),
					pinned(2, uniqueKeys(laneKeys(boundary.lanes, func(lane policyLane) solana.PublicKey { return lane.market }))...),
					pinned(8, uniqueKeys(laneKeys(boundary.lanes, func(lane policyLane) solana.PublicKey { return lane.debtCustody }))...),
					obligationOwnedByVault(boundary),
				},
				DataConstraints: []squads.DataConstraintView{sliceEquals(DiscriminatorBorrowDebt)},
			},
			{
				ProgramID: boundary.klendProgram,
				AccountConstraints: []squads.AccountConstraintView{
					pinned(0, boundary.vault),
					pinned(2, uniqueKeys(laneKeys(boundary.lanes, func(lane policyLane) solana.PublicKey { return lane.market }))...),
					pinned(6, uniqueKeys(laneKeys(boundary.lanes, func(lane policyLane) solana.PublicKey { return lane.debtCustody }))...),
					obligationOwnedByVault(boundary),
				},
				DataConstraints: []squads.DataConstraintView{sliceEquals(DiscriminatorRepayDebt)},
			},
		}, nil
	case FamilySwap:
		return []squads.InstructionConstraintView{
			swapConstraint(boundary,
				[]solana.PublicKey{boundary.usdcCustody, boundary.usdsCustody},
				[]solana.PublicKey{boundary.onycCustody, boundary.primeCustody}),
			swapConstraint(boundary,
				[]solana.PublicKey{boundary.usdcCustody, boundary.pyusdCustody},
				[]solana.PublicKey{boundary.primeCustody, boundary.syrupUsdcCustody}),
			swapConstraint(boundary,
				[]solana.PublicKey{boundary.onycCustody, boundary.primeCustody},
				[]solana.PublicKey{boundary.usdcCustody, boundary.usdsCustody}),
			swapConstraint(boundary,
				[]solana.PublicKey{boundary.primeCustody, boundary.syrupUsdcCustody},
				[]solana.PublicKey{boundary.usdcCustody, boundary.pyusdCustody}),
		}, nil
	}
	return nil, fmt.Errorf("unknown policy family %q", family)
}

type policyLane struct {
	market, obligation, collateralReserve, collateralCustody,
	debtReserve, debtCustody, debtTokenProgram solana.PublicKey
}

type policyBoundary struct {
	vault, klendProgram, jupiterProgram,
	usdcCustody, pyusdCustody, usdsCustody,
	onycCustody, primeCustody, syrupUsdcCustody solana.PublicKey
	lanes []policyLane
}

func collateralConstraint(boundary policyBoundary, discriminator [8]byte) squads.InstructionConstraintView {
	return squads.InstructionConstraintView{
		ProgramID: boundary.klendProgram,
		AccountConstraints: []squads.AccountConstraintView{
			pinned(0, boundary.vault),
			pinned(4, uniqueKeys(laneKeys(boundary.lanes, func(lane policyLane) solana.PublicKey { return lane.collateralReserve }))...),
			pinned(9, uniqueKeys(laneKeys(boundary.lanes, func(lane policyLane) solana.PublicKey { return lane.collateralCustody }))...),
			obligationOwnedByVault(boundary),
		},
		DataConstraints: []squads.DataConstraintView{sliceEquals(discriminator)},
	}
}

func swapConstraint(boundary policyBoundary, sources, destinations []solana.PublicKey) squads.InstructionConstraintView {
	return squads.InstructionConstraintView{
		ProgramID: boundary.jupiterProgram,
		AccountConstraints: []squads.AccountConstraintView{
			pinned(2, boundary.vault),
			pinned(3, sources...),
			pinned(6, destinations...),
		},
		DataConstraints: []squads.DataConstraintView{{
			DataOffset: 0,
			DataValue: squads.DataValueView{Kind: 1, U16: binary.LittleEndian.Uint16(
				JupiterSharedAccountsRouteDiscriminator[:2])},
			Operator: squads.OpEquals,
		}},
	}
}

func obligationOwnedByVault(boundary policyBoundary) squads.AccountConstraintView {
	vault := boundary.vault
	return squads.AccountConstraintView{
		AccountIndex: 1,
		Owner:        &boundary.klendProgram,
		AccountData: []squads.DataConstraintView{{
			DataOffset: 64,
			DataValue:  squads.DataValueView{Kind: 5, Bytes: append([]byte(nil), vault[:]...)},
			Operator:   squads.OpEquals,
		}},
	}
}

func pinned(accountIndex uint8, keys ...solana.PublicKey) squads.AccountConstraintView {
	return squads.AccountConstraintView{AccountIndex: accountIndex, Pubkeys: keys}
}

func sliceEquals(discriminator [8]byte) squads.DataConstraintView {
	return squads.DataConstraintView{
		DataOffset: 0,
		DataValue:  squads.DataValueView{Kind: 5, Bytes: append([]byte(nil), discriminator[:]...)},
		Operator:   squads.OpEquals,
	}
}

func laneKeys(lanes []policyLane, pick func(policyLane) solana.PublicKey) []solana.PublicKey {
	keys := make([]solana.PublicKey, 0, len(lanes))
	for _, lane := range lanes {
		keys = append(keys, pick(lane))
	}
	return keys
}

func uniqueKeys(keys []solana.PublicKey) []solana.PublicKey {
	unique := make([]solana.PublicKey, 0, len(keys))
	for _, key := range keys {
		duplicate := false
		for _, existing := range unique {
			if existing == key {
				duplicate = true
				break
			}
		}
		if !duplicate {
			unique = append(unique, key)
		}
	}
	return unique
}

// CurrentPolicyMatches is the Go port of current_policy_matches: the on-chain
// policy account must be a canonical hookless ProgramInteraction policy under
// the expected PDA, delegating to exactly this signer with threshold 1, and
// its payload must equal the canonical constraint set.
func CurrentPolicyMatches(account *chain.Account, policy PolicyConfig, delegate solana.PublicKey, expected []squads.InstructionConstraintView, expectedVaultIndex uint8) (bool, error) {
	current, err := squads.DecodeCanonicalPolicy(account)
	if err != nil {
		return false, err
	}
	if current == nil {
		return false, nil
	}
	return current.PolicySeed == policy.Seed &&
		current.PolicyAccount == policy.Account &&
		current.DelegatedSigner == delegate &&
		current.Threshold == 1 &&
		current.Payload.VaultIndex == expectedVaultIndex &&
		len(current.Payload.SpendingLimits) == 0 &&
		squads.ConstraintsEqual(current.Payload.Constraints, expected), nil
}

// PolicyDataHash hashes the policy account bytes for the persisted binding.
func PolicyDataHash(data []byte) string {
	digest := sha256.Sum256(data)
	return fmt.Sprintf("%x", digest[:])
}
