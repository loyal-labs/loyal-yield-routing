package squads

import (
	"context"
	"encoding/binary"
	"errors"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// Allow is the constraint that admits one instruction: its program, its
// leading data bytes, and for each account slot the keys it may hold (nil: any
// key). Program packages call it with their instruction's own account order,
// so a builder and the constraint that admits it cannot disagree.
func Allow(program solana.PublicKey, data []byte, slots [][]solana.PublicKey) InstructionConstraintView {
	out := InstructionConstraintView{ProgramID: program, DataConstraints: []DataConstraintView{{
		DataValue: DataValueView{Kind: 5, Bytes: append([]byte(nil), data...)}, Operator: OpEquals,
	}}}
	for index, keys := range slots {
		if keys != nil {
			out.AccountConstraints = append(out.AccountConstraints, AccountConstraintView{AccountIndex: uint8(index), Pubkeys: keys})
		}
	}
	return out
}

// PolicyApply installs one policy and removes the policies it replaces in a
// single execute_settings_transaction_sync: PolicyCreate at Seed, then one
// PolicyRemove per replaced policy. Signer is the smart account's one signer;
// RentPayer funds the new policy and receives nothing back.
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
	policy, err := p.Policy()
	if err != nil {
		return Instruction{}, err
	}
	if p.Seed == 0 || len(p.Constraints) == 0 {
		return Instruction{}, errors.New("policy apply needs a seed and at least one constraint")
	}
	data := append([]byte(nil), ExecuteSettingsTransactionSyncDiscriminator[:]...)
	data = append(data, syncSignerCount)
	data = binary.LittleEndian.AppendUint32(data, uint32(1+len(p.Replace)))
	if data, err = appendLegacyPolicyCreate(data, p.Seed, p.VaultIndex, p.Constraints, p.Delegate); err != nil {
		return Instruction{}, err
	}
	accounts := []solana.AccountMeta{
		{PublicKey: p.Settings, IsWritable: true},
		{PublicKey: p.RentPayer, IsWritable: true, IsSigner: true},
		{PublicKey: solana.SystemProgramID},
		{PublicKey: ProgramID},
		{PublicKey: p.Signer, IsSigner: true},
		{PublicKey: policy, IsWritable: true},
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
		view, err := DecodeCanonicalPolicy(&accounts[i])
		if err != nil {
			view = nil
		}
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
