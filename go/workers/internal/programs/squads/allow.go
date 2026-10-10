package squads

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

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

// Unpinned is Any for every key, for an instruction's program and sysvar
// accounts when its own program checks them.
func Unpinned(solana.PublicKey) Slot { return Any }

// Pinned is Pin of the one key, for program and sysvar accounts a policy pins.
func Pinned(key solana.PublicKey) Slot { return Pin(key) }

// Allow is the constraint that admits one instruction: its program, its
// leading data bytes, and what each account slot admits. Program packages call
// it with their instruction's own account order, so a builder and the
// constraint that admits it cannot disagree. A slot that is neither Any nor
// constrained is a literal that forgot a field, and Allow panics on it.
func Allow(program solana.PublicKey, data []byte, slots []Slot) InstructionConstraintView {
	return AllowData(program, []DataConstraintView{DataBytes(0, data)}, slots)
}

// AllowData is Allow over data predicates, in the order the policy states
// them, for an instruction whose policy also bounds its arguments.
func AllowData(program solana.PublicKey, data []DataConstraintView, slots []Slot) InstructionConstraintView {
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
	View    *PolicyAccountView // nil when the policy is not a canonical hookless one
}

// Policies lists every policy account installed on settings.
func Policies(ctx context.Context, c *chain.Client, settings solana.PublicKey) ([]Installed, error) {
	_, accounts, err := c.ProgramAccounts(ctx, ProgramID, []rpc.RPCFilter{
		{Memcmp: &rpc.RPCFilterMemcmp{Offset: 0, Bytes: PolicyDiscriminator[:]}},
		{Memcmp: &rpc.RPCFilterMemcmp{Offset: 8, Bytes: settings[:]}},
	}, rpc.CommitmentConfirmed, 0)
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

// FindPolicy is the installed policy whose delegate and constraints equal the
// given ones. The chain is where a policy is authorized, so this is the one
// place a worker learns which policy account to execute through.
func FindPolicy(installed []Installed, delegate solana.PublicKey, constraints []InstructionConstraintView) (solana.PublicKey, bool) {
	for _, policy := range installed {
		if policy.View != nil && policy.View.DelegatedSigner == delegate && ConstraintsEqual(policy.View.Payload.Constraints, constraints) {
			return policy.Account, true
		}
	}
	return solana.PublicKey{}, false
}
