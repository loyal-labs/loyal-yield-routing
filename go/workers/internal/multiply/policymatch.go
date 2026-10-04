package multiply

// Faithful port of the Squads ProgramInteraction policy account decoder and
// the canonical Earn MAX constraint builder:
//   crates/loyal-actions/src/detection.rs
//     decode_program_interaction_policy_account and its readers,
//   crates/loyal-actions/src/earn_max.rs
//     earn_max_policy_constraints,
//   crates/loyal-actions/src/squads.rs
//     semantic_program_interaction_constraints,
//   crates/loyal-fleet-worker/src/multiply/policy.rs
//     current_policy_matches.
// The comparison is structural over the decoded payload view, exactly like
// canonical_policy_payload_matches in the Rust worker.

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/squadspolicy"
)

const squadsFullPermissionsMask = uint8(7)

type DataOperatorView = squadspolicy.DataOperatorView
type DataValueView = squadspolicy.DataValueView
type DataConstraintView = squadspolicy.DataConstraintView
type AccountConstraintView = squadspolicy.AccountConstraintView
type InstructionConstraintView = squadspolicy.InstructionConstraintView
type SpendingLimitView = squadspolicy.SpendingLimitView
type PolicyPayloadView = squadspolicy.PolicyPayloadView

const (
	OpEquals               = squadspolicy.OpEquals
	OpNotEquals            = squadspolicy.OpNotEquals
	OpGreaterThan          = squadspolicy.OpGreaterThan
	OpGreaterThanOrEqualTo = squadspolicy.OpGreaterThanOrEqualTo
	OpLessThan             = squadspolicy.OpLessThan
	OpLessThanOrEqualTo    = squadspolicy.OpLessThanOrEqualTo
)

// PolicyAccountView mirrors SquadsProgramInteractionPolicyAccountView.
type PolicyAccountView struct {
	Settings        solana.PublicKey
	PolicySeed      uint64
	PolicyAccount   solana.PublicKey
	DelegatedSigner solana.PublicKey
	Threshold       uint16
	Payload         PolicyPayloadView
}

// DecodeProgramInteractionPolicyAccount is the Go port of
// decode_program_interaction_policy_account: nil means "not a canonical
// hookless ProgramInteraction policy" (which the executor treats as
// mismatch, never as authority to proceed).
func DecodeProgramInteractionPolicyAccount(data []byte) (*PolicyAccountView, error) {
	if len(data) > 64<<10 {
		return nil, errors.New("policy account exceeds payload limit")
	}
	h, offset, err := squadspolicy.DecodeHeader(data)
	if err != nil {
		return nil, err
	}
	if h.Kind != 3 {
		return nil, nil
	}
	settings, policySeed, policyBump := h.Settings, h.PolicySeed, h.Bump
	transactionIndex, staleTransactionIndex := h.TransactionIndex, h.StaleTransactionIndex
	signers, permissions, threshold, timeLock := h.Signers, h.Permissions, h.Threshold, h.TimeLock
	var candidates []squadspolicy.Candidate
	for _, compact := range []bool{false, true} {
		if candidate, err := squadspolicy.DecodePayload(data, offset, h.VaultIndex, compact); err == nil {
			candidates = append(candidates, candidate)
		}
	}
	// The Squads action-account PDA does not expose its bump separately here;
	// the Rust reader re-derives it. FindProgramAddress returns the bump, so
	// recompute it explicitly.
	policyAccount, expectedBump := deriveActionAccountWithBump(settings, policySeed)
	if len(signers) != 1 || permissions[0] != squadsFullPermissionsMask ||
		threshold != 1 || timeLock != 0 || staleTransactionIndex > transactionIndex ||
		policyBump != expectedBump {
		return nil, nil
	}
	var valid []PolicyPayloadView
	for _, candidate := range candidates {
		if !candidate.PreHook && !candidate.PostHook && candidate.ExactSpendingLimits &&
			len(candidate.Payload.SpendingLimits) == 0 &&
			candidate.Start >= 0 && !candidate.HasExpiration &&
			compactPubkeyTableIsTight(candidate.Payload) {
			valid = append(valid, candidate.Payload)
		}
	}
	if len(valid) == 0 {
		return nil, nil
	}
	for _, candidate := range valid[1:] {
		if !payloadEqual(candidate, valid[0]) {
			return nil, errors.New("ambiguous ProgramInteraction account encoding")
		}
	}
	return &PolicyAccountView{
		Settings:        settings,
		PolicySeed:      policySeed,
		PolicyAccount:   policyAccount,
		DelegatedSigner: signers[0],
		Threshold:       threshold,
		Payload:         valid[0],
	}, nil
}

func compactPubkeyTableIsTight(payload PolicyPayloadView) bool {
	if len(payload.PubkeyTable) == 0 {
		return true
	}
	seen := map[solana.PublicKey]struct{}{}
	for _, key := range payload.PubkeyTable {
		seen[key] = struct{}{}
	}
	referenced := map[solana.PublicKey]struct{}{}
	for _, constraint := range payload.Constraints {
		referenced[constraint.ProgramID] = struct{}{}
		for _, account := range constraint.AccountConstraints {
			if account.Owner != nil {
				referenced[*account.Owner] = struct{}{}
			}
			for _, key := range account.Pubkeys {
				referenced[key] = struct{}{}
			}
		}
	}
	return len(referenced) == len(seen)
}

func payloadEqual(left, right PolicyPayloadView) bool {
	return left.VaultIndex == right.VaultIndex &&
		constraintsEqual(left.Constraints, right.Constraints)
}

func constraintsEqual(left, right []InstructionConstraintView) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].ProgramID != right[index].ProgramID ||
			!accountConstraintsEqual(left[index].AccountConstraints, right[index].AccountConstraints) ||
			!dataConstraintsEqual(left[index].DataConstraints, right[index].DataConstraints) {
			return false
		}
	}
	return true
}

func accountConstraintsEqual(left, right []AccountConstraintView) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].AccountIndex != right[index].AccountIndex {
			return false
		}
		if (left[index].Owner == nil) != (right[index].Owner == nil) {
			return false
		}
		if left[index].Owner != nil && *left[index].Owner != *right[index].Owner {
			return false
		}
		if !keysEqual(left[index].Pubkeys, right[index].Pubkeys) ||
			!dataConstraintsEqual(left[index].AccountData, right[index].AccountData) {
			return false
		}
	}
	return true
}

func keysEqual(left, right []solana.PublicKey) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func dataConstraintsEqual(left, right []DataConstraintView) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].DataOffset != right[index].DataOffset ||
			left[index].Operator != right[index].Operator ||
			!dataValueEqual(left[index].DataValue, right[index].DataValue) {
			return false
		}
	}
	return true
}

func dataValueEqual(left, right DataValueView) bool {
	if left.Kind != right.Kind {
		return false
	}
	switch left.Kind {
	case 0:
		return left.U8 == right.U8
	case 1:
		return left.U16 == right.U16
	case 2:
		return left.U32 == right.U32
	case 3:
		return left.U64 == right.U64
	case 4:
		return left.U128 == right.U128
	case 5:
		return equalBytes(left.Bytes, right.Bytes)
	}
	return false
}

// CanonicalConstraints is the Go port of earn_max_policy_constraints over the
// semantic contract: literal KLend lane pinning for collateral/debt families
// and Jupiter SharedAccountsRoute lane pinning for swaps. Constraint indexes
// line up with ConstraintIndexes in policy.go.
func CanonicalConstraints(topology *EarnMaxTopology, family PolicyFamily) (constraints []InstructionConstraintView, err error) {
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
		return []InstructionConstraintView{
			collateralConstraint(boundary, DiscriminatorDepositCollateral),
			collateralConstraint(boundary, DiscriminatorWithdrawCollateral),
		}, nil
	case FamilyDebt:
		return []InstructionConstraintView{
			{
				ProgramID: boundary.klendProgram,
				AccountConstraints: []AccountConstraintView{
					pinned(0, boundary.vault),
					pinned(2, uniqueKeys(laneKeys(boundary.lanes, func(lane policyLane) solana.PublicKey { return lane.market }))...),
					pinned(8, uniqueKeys(laneKeys(boundary.lanes, func(lane policyLane) solana.PublicKey { return lane.debtCustody }))...),
					obligationOwnedByVault(boundary),
				},
				DataConstraints: []DataConstraintView{sliceEquals(DiscriminatorBorrowDebt)},
			},
			{
				ProgramID: boundary.klendProgram,
				AccountConstraints: []AccountConstraintView{
					pinned(0, boundary.vault),
					pinned(2, uniqueKeys(laneKeys(boundary.lanes, func(lane policyLane) solana.PublicKey { return lane.market }))...),
					pinned(6, uniqueKeys(laneKeys(boundary.lanes, func(lane policyLane) solana.PublicKey { return lane.debtCustody }))...),
					obligationOwnedByVault(boundary),
				},
				DataConstraints: []DataConstraintView{sliceEquals(DiscriminatorRepayDebt)},
			},
		}, nil
	case FamilySwap:
		return []InstructionConstraintView{
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

func collateralConstraint(boundary policyBoundary, discriminator [8]byte) InstructionConstraintView {
	return InstructionConstraintView{
		ProgramID: boundary.klendProgram,
		AccountConstraints: []AccountConstraintView{
			pinned(0, boundary.vault),
			pinned(4, uniqueKeys(laneKeys(boundary.lanes, func(lane policyLane) solana.PublicKey { return lane.collateralReserve }))...),
			pinned(9, uniqueKeys(laneKeys(boundary.lanes, func(lane policyLane) solana.PublicKey { return lane.collateralCustody }))...),
			obligationOwnedByVault(boundary),
		},
		DataConstraints: []DataConstraintView{sliceEquals(discriminator)},
	}
}

func swapConstraint(boundary policyBoundary, sources, destinations []solana.PublicKey) InstructionConstraintView {
	return InstructionConstraintView{
		ProgramID: boundary.jupiterProgram,
		AccountConstraints: []AccountConstraintView{
			pinned(2, boundary.vault),
			pinned(3, sources...),
			pinned(6, destinations...),
		},
		DataConstraints: []DataConstraintView{{
			DataOffset: 0,
			DataValue: DataValueView{Kind: 1, U16: binary.LittleEndian.Uint16(
				JupiterSharedAccountsRouteDiscriminator[:2])},
			Operator: OpEquals,
		}},
	}
}

func obligationOwnedByVault(boundary policyBoundary) AccountConstraintView {
	vault := boundary.vault
	return AccountConstraintView{
		AccountIndex: 1,
		Owner:        &boundary.klendProgram,
		AccountData: []DataConstraintView{{
			DataOffset: 64,
			DataValue:  DataValueView{Kind: 5, Bytes: append([]byte(nil), vault[:]...)},
			Operator:   OpEquals,
		}},
	}
}

func pinned(accountIndex uint8, keys ...solana.PublicKey) AccountConstraintView {
	return AccountConstraintView{AccountIndex: accountIndex, Pubkeys: keys}
}

func sliceEquals(discriminator [8]byte) DataConstraintView {
	return DataConstraintView{
		DataOffset: 0,
		DataValue:  DataValueView{Kind: 5, Bytes: append([]byte(nil), discriminator[:]...)},
		Operator:   OpEquals,
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
func CurrentPolicyMatches(data []byte, policy PolicyConfig, delegate solana.PublicKey, expected []InstructionConstraintView, expectedVaultIndex uint8) (bool, error) {
	current, err := DecodeProgramInteractionPolicyAccount(data)
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
		constraintsEqual(current.Payload.Constraints, expected), nil
}

// PolicyDataHash hashes the policy account bytes for the persisted binding.
func PolicyDataHash(data []byte) string {
	digest := sha256.Sum256(data)
	return fmt.Sprintf("%x", digest[:])
}
