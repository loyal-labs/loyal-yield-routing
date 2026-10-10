package squads

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// Slot is what one account position of a constraint admits: any account, one
// of Keys, or an account whose data matches Data. Owner, when set, also pins
// the account's owner program. The zero Slot admits nothing a policy can say,
// so Allow refuses it: a slot is free only when the literal says Any.
type Slot struct {
	Any   bool
	Keys  []solana.PublicKey
	Data  []DataConstraintView
	Owner *solana.PublicKey
}

// Any is the slot that admits any account.
var Any = Slot{Any: true}

// Pin is the slot that admits only keys.
func Pin(keys ...solana.PublicKey) Slot { return Slot{Keys: keys} }

// Unpinned is Any for every key: a fixed slot the policy leaves to the
// instruction's own program.
func Unpinned(solana.PublicKey) Slot { return Any }

// Pinned is Pin of the one key: a fixed slot the policy pins.
func Pinned(key solana.PublicKey) Slot { return Pin(key) }

// AccountSlot is one account position of an instruction: its key (a
// solana.PublicKey for the instruction, a Slot for the constraint that admits
// it) and its flags. A program package writes each instruction's account order
// once, as a list of these over the key type, so its builder (Metas) and its
// constraint (Slots) read the same list. An Optional account is Anchor's
// optional account: absent (the zero key), it is the program's own id,
// read-only.
type AccountSlot[T any] struct {
	Key                        T
	Writable, Signer, Optional bool
}

func ReadOnly[T any](key T) AccountSlot[T] { return AccountSlot[T]{Key: key} }
func Writable[T any](key T) AccountSlot[T] { return AccountSlot[T]{Key: key, Writable: true} }
func Signing[T any](key T) AccountSlot[T]  { return AccountSlot[T]{Key: key, Signer: true} }
func SigningWritable[T any](key T) AccountSlot[T] {
	return AccountSlot[T]{Key: key, Writable: true, Signer: true}
}
func Optional[T any](key T, writable bool) AccountSlot[T] {
	return AccountSlot[T]{Key: key, Writable: writable, Optional: true}
}

// Metas are program's instruction accounts for slots.
func Metas(program solana.PublicKey, slots []AccountSlot[solana.PublicKey]) []*solana.AccountMeta {
	out := make([]*solana.AccountMeta, len(slots))
	for i, s := range slots {
		key, writable := s.Key, s.Writable
		if s.Optional && key.IsZero() {
			key, writable = program, false
		}
		out[i] = &solana.AccountMeta{PublicKey: key, IsWritable: writable, IsSigner: s.Signer}
	}
	return out
}

// Slots are what program's constraint admits at each of slots. An optional
// slot that pins the zero key (an absent account) admits the program id the
// builder sends for it.
func Slots(program solana.PublicKey, slots []AccountSlot[Slot]) []Slot {
	out := make([]Slot, len(slots))
	for i, s := range slots {
		out[i] = s.Key
		if s.Optional && len(s.Key.Keys) > 0 {
			out[i].Keys = make([]solana.PublicKey, len(s.Key.Keys))
			for j, key := range s.Key.Keys {
				if key.IsZero() {
					key = program
				}
				out[i].Keys[j] = key
			}
		}
	}
	return out
}

// Allow is the constraint that admits one instruction: its program, the
// predicates on its data, in the order the policy states them (the
// discriminator first, then any bound on its arguments), and what each
// account slot admits. Program packages call it with their instruction's own
// account order, so a builder and the constraint that admits it cannot
// disagree. A slot that is neither Any nor constrained is a literal that
// forgot a field, and Allow panics on it.
func Allow(program solana.PublicKey, data []DataConstraintView, slots []Slot) InstructionConstraintView {
	out := InstructionConstraintView{ProgramID: program, DataConstraints: data}
	for index, slot := range slots {
		switch {
		case slot.Any:
		case len(slot.Keys) > 0 && len(slot.Data) == 0:
			out.AccountConstraints = append(out.AccountConstraints, AccountConstraintView{AccountIndex: uint8(index), Pubkeys: slot.Keys, Owner: slot.Owner})
		case len(slot.Keys) == 0 && len(slot.Data) > 0:
			out.AccountConstraints = append(out.AccountConstraints, AccountConstraintView{AccountIndex: uint8(index), AccountData: slot.Data, Owner: slot.Owner})
		default:
			panic(fmt.Sprintf("constraint for %s leaves account %d unset", program, index))
		}
	}
	return out
}

// DataBytes is the predicate that instruction data at offset equals value.
func DataBytes(offset uint64, value []byte) DataConstraintView {
	return DataConstraintView{DataOffset: offset, DataValue: DataValueView{Kind: 5, Bytes: append([]byte(nil), value...)}, Operator: OpEquals}
}

// DataU8 is the predicate that the byte at offset compares to value by op.
func DataU8(offset uint64, op DataOperatorView, value uint8) DataConstraintView {
	return DataConstraintView{DataOffset: offset, DataValue: DataValueView{Kind: 0, U8: value}, Operator: op}
}

// DataU16 is the predicate that the little-endian u16 at offset compares to
// value by op.
func DataU16(offset uint64, op DataOperatorView, value uint16) DataConstraintView {
	return DataConstraintView{DataOffset: offset, DataValue: DataValueView{Kind: 1, U16: value}, Operator: op}
}

// DataU64 is the predicate that the little-endian u64 at offset compares to
// value by op.
func DataU64(offset uint64, op DataOperatorView, value uint64) DataConstraintView {
	return DataConstraintView{DataOffset: offset, DataValue: DataValueView{Kind: 3, U64: value}, Operator: op}
}

// PolicyApply installs one policy and removes the policies it replaces in a
// single execute_settings_transaction_sync: PolicyCreate at Seed (none when
// Constraints is empty), then one PolicyRemove per replaced policy. Signer is
// the smart account's one signer; RentPayer funds the new policy.
type PolicyApply struct {
	Settings, RentPayer, Signer, Delegate solana.PublicKey
	Seed                                  uint64
	VaultIndex                            uint8
	Constraints                           []InstructionConstraintView
	Replace                               []solana.PublicKey
}

// Policy is the address the applied policy will have.
func (p PolicyApply) Policy() (solana.PublicKey, error) {
	policy, _, err := PolicyAddress(p.Settings, p.Seed)
	return policy, err
}

// Instruction is the settings instruction: accounts are settings (w), rent
// payer (w, s), system program, the Squads program, the signer (s), then the
// new and replaced policies (w).
func (p PolicyApply) Instruction() (Instruction, error) {
	creates := 0
	if len(p.Constraints) > 0 {
		creates = 1
	}
	if creates+len(p.Replace) == 0 {
		return Instruction{}, errors.New("policy apply creates and removes nothing")
	}
	data := append([]byte(nil), ExecuteSettingsTransactionSyncDiscriminator[:]...)
	data = append(data, syncSignerCount)
	data = binary.LittleEndian.AppendUint32(data, uint32(creates+len(p.Replace)))
	accounts := []solana.AccountMeta{
		{PublicKey: p.Settings, IsWritable: true},
		{PublicKey: p.RentPayer, IsWritable: true, IsSigner: true},
		{PublicKey: solana.SystemProgramID},
		{PublicKey: ProgramID},
		{PublicKey: p.Signer, IsSigner: true},
	}
	if creates == 1 {
		policy, err := p.Policy()
		if err != nil {
			return Instruction{}, err
		}
		if data, err = appendLegacyPolicyCreate(data, p.Seed, p.VaultIndex, p.Constraints, p.Delegate); err != nil {
			return Instruction{}, err
		}
		accounts = append(accounts, solana.AccountMeta{PublicKey: policy, IsWritable: true})
	}
	for _, old := range p.Replace {
		data = append(append(data, 9), old[:]...) // SettingsAction::PolicyRemove
		accounts = append(accounts, solana.AccountMeta{PublicKey: old, IsWritable: true})
	}
	data = append(data, 0) // memo
	return Instruction{ProgramID: ProgramID, Accounts: accounts, Data: data}, nil
}

// NextPolicySeed is the seed the next PolicyCreate on settings must use.
func NextPolicySeed(s Settings) uint64 {
	if s.PolicySeed == nil {
		return 1
	}
	return *s.PolicySeed + 1
}

// Installed is one ProgramInteraction policy installed on a Settings.
type Installed struct {
	Account solana.PublicKey
	View    *PolicyAccountView // nil when the policy is not a canonical one (DecodeCanonicalPolicy)
}

// Policies lists every policy account installed on settings, read at a slot
// no older than minSlot (none when zero).
func Policies(ctx context.Context, c *chain.Client, settings solana.PublicKey, minSlot uint64) ([]Installed, error) {
	_, accounts, err := c.ProgramAccounts(ctx, ProgramID, []rpc.RPCFilter{
		{Memcmp: &rpc.RPCFilterMemcmp{Offset: 0, Bytes: PolicyDiscriminator[:]}},
		{Memcmp: &rpc.RPCFilterMemcmp{Offset: 8, Bytes: settings[:]}},
	}, rpc.CommitmentConfirmed, minSlot)
	if err != nil {
		return nil, err
	}
	out := make([]Installed, 0, len(accounts))
	for i := range accounts {
		view, _ := DecodeCanonicalPolicy(&accounts[i])
		out = append(out, Installed{Account: accounts[i].Key, View: view})
	}
	return out, nil
}

// FindPolicy is the installed account on settings, delegated to delegate,
// whose canonical policy equals want: vault index, constraints and spending
// limits, with Squads' usage counters ignored. It also returns how many
// accounts match, and finds the policy only when exactly one does, so a
// worker never splits its spending across two equal accounts. That is all
// uniqueness gives: an account that does not decode canonically, or that
// differs from want (the same constraints without limits, say), is invisible
// here, so this bounds nothing the delegate key can do through such a twin.
// Only the Settings signer controls what is installed.
func FindPolicy(installed []Installed, settings, delegate solana.PublicKey, want Policy) (Installed, int) {
	var found Installed
	matches := 0
	for _, policy := range installed {
		if policy.View != nil && policy.View.Settings == settings && policy.View.DelegatedSigner == delegate && policy.View.Payload.Policy.Equal(want) {
			found = policy
			matches++
		}
	}
	if matches != 1 {
		return Installed{}, matches
	}
	return found, matches
}

// Admits reports whether constraint admits ix as Squads checks one
// ProgramInteraction constraint: ix calls its program, the account at each
// constrained index is one of its keys or holds data its predicates admit
// (and has its owner, when one is set), and ix's data satisfies every data
// predicate. account is the state of an account a data or owner predicate
// reads; nil, or a nil result, admits nothing there.
func Admits(constraint InstructionConstraintView, ix Instruction, account func(solana.PublicKey) *chain.Account) bool {
	if constraint.ProgramID != ix.ProgramID {
		return false
	}
	for _, c := range constraint.AccountConstraints {
		if int(c.AccountIndex) >= len(ix.Accounts) {
			return false
		}
		key := ix.Accounts[c.AccountIndex].PublicKey
		if c.Pubkeys != nil && !slices.Contains(c.Pubkeys, key) {
			return false
		}
		if c.Pubkeys == nil || c.Owner != nil {
			var state *chain.Account
			if account != nil {
				state = account(key)
			}
			if state == nil || c.Owner != nil && state.Owner != *c.Owner || !predicatesHold(c.AccountData, state.Data) {
				return false
			}
		}
	}
	return predicatesHold(constraint.DataConstraints, ix.Data)
}

// predicatesHold reports whether data satisfies every predicate: the
// little-endian integer at its offset compares to its value by its operator,
// or the bytes there equal (or, by OpNotEquals, differ from) its bytes. Data
// too short for a predicate fails it.
func predicatesHold(predicates []DataConstraintView, data []byte) bool {
	for _, d := range predicates {
		v := d.DataValue
		size := map[uint8]int{0: 1, 1: 2, 2: 4, 3: 8, 4: 16, 5: len(v.Bytes)}[v.Kind]
		if v.Kind > 5 || d.DataOffset > uint64(len(data)) || uint64(size) > uint64(len(data))-d.DataOffset {
			return false
		}
		at := data[d.DataOffset : d.DataOffset+uint64(size)]
		var order int
		switch v.Kind {
		case 0:
			order = cmp.Compare(at[0], v.U8)
		case 1:
			order = cmp.Compare(binary.LittleEndian.Uint16(at), v.U16)
		case 2:
			order = cmp.Compare(binary.LittleEndian.Uint32(at), v.U32)
		case 3:
			order = cmp.Compare(binary.LittleEndian.Uint64(at), v.U64)
		case 4:
			order = bytes.Compare(reversed(at), reversed(v.U128[:]))
		case 5:
			if d.Operator != OpEquals && d.Operator != OpNotEquals {
				return false
			}
			order = bytes.Compare(at, v.Bytes)
		}
		if !map[DataOperatorView]bool{OpEquals: order == 0, OpNotEquals: order != 0, OpGreaterThan: order > 0,
			OpGreaterThanOrEqualTo: order >= 0, OpLessThan: order < 0, OpLessThanOrEqualTo: order <= 0}[d.Operator] {
			return false
		}
	}
	return true
}

// reversed is a little-endian integer's bytes in big-endian order, which
// compare as the integers do.
func reversed(le []byte) []byte {
	out := slices.Clone(le)
	slices.Reverse(out)
	return out
}
