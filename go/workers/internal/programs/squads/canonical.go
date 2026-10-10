package squads

import (
	"bytes"
	"encoding/binary"
	"errors"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/solana-foundation/solana-go/v2"
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

// DecodeCanonicalPolicy is the Go port of loyal-actions detection.rs
// decode_program_interaction_policy_account: nil means "not a canonical
// ProgramInteraction policy", which callers treat as a mismatch, never as
// authority to proceed. Canonical is one full-permission signer, threshold 1,
// no time lock, hooks or expiration, and the policy's own bump. What the
// policy authorizes, spending limits included, is Payload.Policy, which a
// caller compares to the policy it executes through (Policy.Equal,
// FindPolicy).
func DecodeCanonicalPolicy(account *chain.Account) (*PolicyAccountView, error) {
	if account == nil || account.Owner != ProgramID || account.Executable {
		return nil, errors.New("policy account is absent or not owned by Squads")
	}
	data := account.Data
	if len(data) > 64<<10 {
		return nil, errors.New("policy account exceeds payload limit")
	}
	h, offset, err := decodeHeader(data)
	if err != nil {
		return nil, err
	}
	if h.Kind != 3 {
		return nil, nil
	}
	var candidates []FullPayload
	for _, compact := range []bool{false, true} {
		if candidate, err := decodePayload(data, offset, h.VaultIndex, compact); err == nil {
			candidates = append(candidates, candidate)
		}
	}
	policyAccount, expectedBump, err := PolicyAddress(h.Settings, h.PolicySeed)
	if err != nil {
		return nil, err
	}
	if len(h.Signers) != 1 || h.Permissions[0] != FullPermissions || h.Threshold != 1 || h.TimeLock != 0 || h.StaleTransactionIndex > h.TransactionIndex || h.Bump != expectedBump {
		return nil, nil
	}
	var valid []PolicyPayloadView
	for _, candidate := range candidates {
		if !candidate.PreHook && !candidate.PostHook && candidate.Start >= 0 && !candidate.HasExpiration && compactPubkeyTableIsTight(candidate.Payload) {
			valid = append(valid, candidate.Payload)
		}
	}
	if len(valid) == 0 {
		return nil, nil
	}
	for _, candidate := range valid[1:] {
		if !candidate.Policy.Equal(valid[0].Policy) {
			return nil, errors.New("ambiguous ProgramInteraction account encoding")
		}
	}
	return &PolicyAccountView{Settings: h.Settings, PolicySeed: h.PolicySeed, PolicyAccount: policyAccount, DelegatedSigner: h.Signers[0], Threshold: h.Threshold, Payload: valid[0]}, nil
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
	for _, limit := range payload.SpendingLimits {
		referenced[limit.Mint] = struct{}{}
	}
	return len(referenced) == len(seen)
}

// ConstraintsEqual compares decoded constraint semantics: program, ordered
// account clauses (index, owner, pinned keys or data predicates) and data
// predicates. Compact pubkey-table indexes are already resolved.
func ConstraintsEqual(left, right []InstructionConstraintView) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].ProgramID != right[index].ProgramID || !accountConstraintsEqual(left[index].AccountConstraints, right[index].AccountConstraints) || !dataConstraintsEqual(left[index].DataConstraints, right[index].DataConstraints) {
			return false
		}
	}
	return true
}

// SpendingLimitsEqual compares spending limits by what they allow: mint,
// period, amount per period and per use, accumulation, exact quantity and
// expiration. Start is set when the policy is created and Squads re-windows
// it, so a literal cannot state it.
func SpendingLimitsEqual(left, right []SpendingLimitView) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		l, r := left[index], right[index]
		if l.Mint != r.Mint || l.Period != r.Period || l.CustomPeriod != r.CustomPeriod || l.MaxPerPeriod != r.MaxPerPeriod ||
			l.Accumulate != r.Accumulate || l.MaxPerUse != r.MaxPerUse || l.ExactQuantity != r.ExactQuantity ||
			(l.Expiration == nil) != (r.Expiration == nil) || l.Expiration != nil && *l.Expiration != *r.Expiration {
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
		l, r := left[index], right[index]
		if l.AccountIndex != r.AccountIndex || (l.Owner == nil) != (r.Owner == nil) || l.Owner != nil && *l.Owner != *r.Owner || len(l.Pubkeys) != len(r.Pubkeys) || !dataConstraintsEqual(l.AccountData, r.AccountData) {
			return false
		}
		for i := range l.Pubkeys {
			if l.Pubkeys[i] != r.Pubkeys[i] {
				return false
			}
		}
	}
	return true
}

func dataConstraintsEqual(left, right []DataConstraintView) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		l, r := left[index].DataValue, right[index].DataValue
		if left[index].DataOffset != right[index].DataOffset || left[index].Operator != right[index].Operator || l.Kind != r.Kind {
			return false
		}
		switch l.Kind {
		case 0:
			if l.U8 != r.U8 {
				return false
			}
		case 1:
			if l.U16 != r.U16 {
				return false
			}
		case 2:
			if l.U32 != r.U32 {
				return false
			}
		case 3:
			if l.U64 != r.U64 {
				return false
			}
		case 4:
			if l.U128 != r.U128 {
				return false
			}
		case 5:
			if !bytes.Equal(l.Bytes, r.Bytes) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// appendLegacyPolicyCreate writes one PolicyCreate settings action carrying
// the deployed LegacyProgramInteraction payload (enum index 3): hookless, no
// spending limits, one full-permission delegated signer, threshold 1, no time
// lock, start or expiration. It is loyal-actions squads.rs
// serialize_settings_actions for that action.
func appendLegacyPolicyCreate(out []byte, seed uint64, accountIndex uint8, constraints []InstructionConstraintView, delegate solana.PublicKey) ([]byte, error) {
	out = append(out, 7)                              // SettingsAction::PolicyCreate
	out = binary.LittleEndian.AppendUint64(out, seed) // seed
	out = append(out, 3, accountIndex)                // LegacyProgramInteraction
	out = binary.LittleEndian.AppendUint32(out, uint32(len(constraints)))
	for _, constraint := range constraints {
		out = append(out, constraint.ProgramID[:]...)
		out = binary.LittleEndian.AppendUint32(out, uint32(len(constraint.AccountConstraints)))
		for _, account := range constraint.AccountConstraints {
			out = append(out, account.AccountIndex)
			if account.Pubkeys != nil {
				out = append(out, 0)
				out = binary.LittleEndian.AppendUint32(out, uint32(len(account.Pubkeys)))
				for _, key := range account.Pubkeys {
					out = append(out, key[:]...)
				}
			} else {
				out = append(out, 1)
				var err error
				if out, err = appendDataConstraints(out, account.AccountData); err != nil {
					return nil, err
				}
			}
			if account.Owner == nil {
				out = append(out, 0)
			} else {
				out = append(append(out, 1), account.Owner[:]...)
			}
		}
		var err error
		if out, err = appendDataConstraints(out, constraint.DataConstraints); err != nil {
			return nil, err
		}
	}
	out = append(out, 0, 0)                        // pre_hook, post_hook
	out = binary.LittleEndian.AppendUint32(out, 0) // spending_limits
	out = binary.LittleEndian.AppendUint32(out, 1) // signers
	out = append(append(out, delegate[:]...), FullPermissions)
	out = binary.LittleEndian.AppendUint16(out, 1) // threshold
	out = binary.LittleEndian.AppendUint32(out, 0) // time_lock
	return append(out, 0, 0), nil                  // start_timestamp, expiration_args
}

func appendDataConstraints(out []byte, constraints []DataConstraintView) ([]byte, error) {
	out = binary.LittleEndian.AppendUint32(out, uint32(len(constraints)))
	for _, constraint := range constraints {
		out = binary.LittleEndian.AppendUint64(out, constraint.DataOffset)
		value := constraint.DataValue
		out = append(out, value.Kind)
		switch value.Kind {
		case 0:
			out = append(out, value.U8)
		case 1:
			out = binary.LittleEndian.AppendUint16(out, value.U16)
		case 2:
			out = binary.LittleEndian.AppendUint32(out, value.U32)
		case 3:
			out = binary.LittleEndian.AppendUint64(out, value.U64)
		case 4:
			out = append(out, value.U128[:]...)
		case 5:
			out = binary.LittleEndian.AppendUint32(out, uint32(len(value.Bytes)))
			out = append(out, value.Bytes...)
		default:
			return nil, errors.New("unknown policy data value kind")
		}
		out = append(out, uint8(constraint.Operator))
	}
	return out, nil
}
