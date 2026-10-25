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

type Slot struct {
	kind  slotKind
	keys  []solana.PublicKey
	data  []DataConstraintView
	owner *solana.PublicKey
}

type slotKind uint8

const (
	slotUnset slotKind = iota
	slotAny
	slotPinned
	slotMatched
)

var Any = Slot{kind: slotAny}

func Pin(keys ...solana.PublicKey) Slot { return Slot{kind: slotPinned, keys: keys} }

func Match(owner solana.PublicKey, data ...DataConstraintView) Slot {
	return Slot{kind: slotMatched, data: data, owner: &owner}
}

func Unpinned(solana.PublicKey) Slot { return Any }

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

func Slots(program solana.PublicKey, slots []AccountSlot[Slot]) []Slot {
	out := make([]Slot, len(slots))
	for i, s := range slots {
		out[i] = s.Key
		if s.Optional && len(s.Key.keys) > 0 {
			out[i].keys = make([]solana.PublicKey, len(s.Key.keys))
			for j, key := range s.Key.keys {
				if key.IsZero() {
					key = program
				}
				out[i].keys[j] = key
			}
		}
	}
	return out
}

func Allow(program solana.PublicKey, data []DataConstraintView, slots []Slot) InstructionConstraintView {
	out := InstructionConstraintView{ProgramID: program, DataConstraints: data}
	for index, slot := range slots {
		switch {
		case slot.kind == slotAny:
		case slot.kind == slotPinned && len(slot.keys) > 0:
			out.AccountConstraints = append(out.AccountConstraints, AccountConstraintView{AccountIndex: uint8(index), Pubkeys: slot.keys, Owner: slot.owner})
		case slot.kind == slotMatched && len(slot.data) > 0:
			out.AccountConstraints = append(out.AccountConstraints, AccountConstraintView{AccountIndex: uint8(index), AccountData: slot.data, Owner: slot.owner})
		default:
			panic(fmt.Sprintf("constraint for %s leaves account %d unset", program, index))
		}
	}
	return out
}

func DataBytes(offset uint64, value []byte) DataConstraintView {
	return DataConstraintView{DataOffset: offset, DataValue: DataValueView{Kind: ValueBytes, Bytes: append([]byte(nil), value...)}, Operator: OpEquals}
}

func DataU8(offset uint64, op DataOperatorView, value uint8) DataConstraintView {
	return DataConstraintView{DataOffset: offset, DataValue: DataValueView{Kind: ValueU8, U8: value}, Operator: op}
}

func DataU16(offset uint64, op DataOperatorView, value uint16) DataConstraintView {
	return DataConstraintView{DataOffset: offset, DataValue: DataValueView{Kind: ValueU16, U16: value}, Operator: op}
}

func DataU64(offset uint64, op DataOperatorView, value uint64) DataConstraintView {
	return DataConstraintView{DataOffset: offset, DataValue: DataValueView{Kind: ValueU64, U64: value}, Operator: op}
}

type PolicyApply struct {
	Settings, RentPayer, Signer, Delegate solana.PublicKey
	Seed                                  uint64
	VaultIndex                            uint8
	Constraints                           []InstructionConstraintView
	SpendingLimits                        []SpendingLimitView
	Replace                               []solana.PublicKey
}

func (p PolicyApply) Policy() (solana.PublicKey, error) {
	policy, _, err := PolicyAddress(p.Settings, p.Seed)
	return policy, err
}

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
		if data, err = appendLegacyPolicyCreate(data, p.Seed, p.VaultIndex, p.Constraints, p.SpendingLimits, p.Delegate); err != nil {
			return Instruction{}, err
		}
		accounts = append(accounts, solana.AccountMeta{PublicKey: policy, IsWritable: true})
	}
	for _, old := range p.Replace {
		data = append(append(data, settingsActionPolicyRemove), old[:]...)
		accounts = append(accounts, solana.AccountMeta{PublicKey: old, IsWritable: true})
	}
	data = append(data, noMemo)
	return Instruction{ProgramID: ProgramID, Accounts: accounts, Data: data}, nil
}

func NextPolicySeed(s Settings) uint64 {
	if s.PolicySeed == nil {
		return 1
	}
	return *s.PolicySeed + 1
}

type Installed struct {
	Account solana.PublicKey
	View    *PolicyAccountView
	Raw     *chain.Account
}

func Policies(ctx context.Context, c *chain.Client, settings solana.PublicKey, minSlot uint64) ([]Installed, error) {
	slot, read, err := c.Accounts(ctx, []solana.PublicKey{settings}, rpc.CommitmentConfirmed, minSlot)
	if err != nil {
		return nil, err
	}
	decoded, err := DecodeSettings(read[0])
	if err != nil {
		return nil, fmt.Errorf("settings %s: %w", settings, err)
	}
	if decoded.PolicySeed == nil || *decoded.PolicySeed == 0 {
		return nil, nil
	}
	addresses := make([]solana.PublicKey, 0, *decoded.PolicySeed)
	for seed := uint64(1); seed <= *decoded.PolicySeed; seed++ {
		address, _, err := PolicyAddress(settings, seed)
		if err != nil {
			return nil, err
		}
		addresses = append(addresses, address)
	}
	_, accounts, err := c.Accounts(ctx, addresses, rpc.CommitmentConfirmed, slot)
	if err != nil {
		return nil, err
	}
	out := make([]Installed, 0, len(accounts))
	for _, account := range accounts {
		if account == nil || account.Owner != ProgramID || len(account.Data) < 40 ||
			!bytes.Equal(account.Data[:8], PolicyDiscriminator[:]) || !bytes.Equal(account.Data[8:40], settings[:]) {
			continue
		}
		view, _ := DecodeCanonicalPolicy(account)
		out = append(out, Installed{Account: account.Key, View: view, Raw: account})
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

func predicatesHold(predicates []DataConstraintView, data []byte) bool {
	for _, d := range predicates {
		v := d.DataValue
		size := map[uint8]int{ValueU8: 1, ValueU16: 2, ValueU32: 4, ValueU64: 8, ValueU128: 16, ValueBytes: len(v.Bytes)}[v.Kind]
		if v.Kind > ValueBytes || d.DataOffset > uint64(len(data)) || uint64(size) > uint64(len(data))-d.DataOffset {
			return false
		}
		at := data[d.DataOffset : d.DataOffset+uint64(size)]
		var order int
		switch v.Kind {
		case ValueU8:
			order = cmp.Compare(at[0], v.U8)
		case ValueU16:
			order = cmp.Compare(binary.LittleEndian.Uint16(at), v.U16)
		case ValueU32:
			order = cmp.Compare(binary.LittleEndian.Uint32(at), v.U32)
		case ValueU64:
			order = cmp.Compare(binary.LittleEndian.Uint64(at), v.U64)
		case ValueU128:
			order = bytes.Compare(reversed(at), reversed(v.U128[:]))
		case ValueBytes:
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

func reversed(le []byte) []byte {
	out := slices.Clone(le)
	slices.Reverse(out)
	return out
}
