package squads

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"

	"github.com/solana-foundation/solana-go/v2"
)

// Instruction is one instruction with resolved accounts.
type Instruction struct {
	ProgramID solana.PublicKey
	Accounts  []solana.AccountMeta
	Data      []byte
}

// ExecuteSync is an execute_transaction_sync_v2 that runs inner instructions
// as the smart account through a ProgramInteraction policy.
type ExecuteSync struct {
	Policy, Signer    solana.PublicKey
	AccountIndex      uint8
	ConstraintIndexes []byte
	Inner             []Instruction
}

// ExecuteTransactionSyncV2 builds execute_transaction_sync_v2 with one signer
// and SyncPayload::Policy(ProgramInteraction(SyncTransaction)). Accounts are
// [policy (w), program, signer (s), transaction accounts...]; the transaction
// accounts are each inner instruction's accounts then its program, merged by
// key, with signer flags cleared (the smart account signs by CPI).
// ConstraintIndexes[i] names the policy constraint that authorizes Inner[i].
func ExecuteTransactionSyncV2(e ExecuteSync) (Instruction, error) {
	if len(e.Inner) == 0 || len(e.Inner) > math.MaxUint8 || len(e.Inner) != len(e.ConstraintIndexes) {
		return Instruction{}, errors.New("squads sync execution needs one constraint index per inner instruction")
	}
	var accounts []solana.AccountMeta
	push := func(meta solana.AccountMeta) (uint8, error) {
		for i := range accounts {
			if accounts[i].PublicKey == meta.PublicKey {
				accounts[i].IsSigner = accounts[i].IsSigner || meta.IsSigner
				accounts[i].IsWritable = accounts[i].IsWritable || meta.IsWritable
				return uint8(i), nil
			}
		}
		if len(accounts) > math.MaxUint8 {
			return 0, errors.New("squads sync transaction exceeds 256 accounts")
		}
		accounts = append(accounts, meta)
		return uint8(len(accounts) - 1), nil
	}
	compiled := []byte{uint8(len(e.Inner))}
	for _, ix := range e.Inner {
		if len(ix.Accounts) > math.MaxUint8 || len(ix.Data) > math.MaxUint16 {
			return Instruction{}, errors.New("squads sync inner instruction overflows its compact encoding")
		}
		indexes := make([]byte, 0, len(ix.Accounts))
		for _, meta := range ix.Accounts {
			index, err := push(meta)
			if err != nil {
				return Instruction{}, err
			}
			indexes = append(indexes, index)
		}
		program, err := push(solana.AccountMeta{PublicKey: ix.ProgramID})
		if err != nil {
			return Instruction{}, err
		}
		compiled = append(compiled, program, uint8(len(indexes)))
		compiled = append(compiled, indexes...)
		compiled = binary.LittleEndian.AppendUint16(compiled, uint16(len(ix.Data)))
		compiled = append(compiled, ix.Data...)
	}
	for i := range accounts {
		accounts[i].IsSigner = false
	}
	data := append([]byte(nil), ExecuteTransactionSyncV2Discriminator[:]...)
	// account_index, num_signers 1, SyncPayload::Policy, PolicyPayload::
	// ProgramInteraction, Some(instruction_constraint_indices).
	data = append(data, e.AccountIndex, 1, 1, 1, 1)
	data = binary.LittleEndian.AppendUint32(data, uint32(len(e.ConstraintIndexes)))
	data = append(data, e.ConstraintIndexes...)
	data = append(data, 1, e.AccountIndex) // TransactionPayload::SyncTransaction { account_index }
	data = binary.LittleEndian.AppendUint32(data, uint32(len(compiled)))
	data = append(data, compiled...)
	metas := make([]solana.AccountMeta, 0, 3+len(accounts))
	metas = append(metas, solana.AccountMeta{PublicKey: e.Policy, IsWritable: true}, solana.AccountMeta{PublicKey: ProgramID}, solana.AccountMeta{PublicKey: e.Signer, IsSigner: true})
	return Instruction{ProgramID: ProgramID, Accounts: append(metas, accounts...), Data: data}, nil
}

// DecodeExecuteTransactionSyncV2 is the inverse of ExecuteTransactionSyncV2:
// it accepts exactly that envelope and resolves each inner instruction's
// program and accounts against the transaction accounts.
func DecodeExecuteTransactionSyncV2(ix Instruction) (ExecuteSync, error) {
	var out ExecuteSync
	bad := errors.New("instruction is not a canonical Squads sync policy execution")
	d := ix.Data
	if ix.ProgramID != ProgramID || len(ix.Accounts) < 3 || ix.Accounts[1].PublicKey != ProgramID || len(d) < 17 ||
		!bytes.Equal(d[:8], ExecuteTransactionSyncV2Discriminator[:]) || !bytes.Equal(d[9:13], []byte{1, 1, 1, 1}) {
		return out, bad
	}
	out.Policy, out.Signer, out.AccountIndex = ix.Accounts[0].PublicKey, ix.Accounts[2].PublicKey, d[8]
	count := int(binary.LittleEndian.Uint32(d[13:17]))
	if count == 0 || count > math.MaxUint8 || len(d) < 17+count+2+4 {
		return out, bad
	}
	out.ConstraintIndexes = bytes.Clone(d[17 : 17+count])
	rest := d[17+count:]
	if rest[0] != 1 || rest[1] != out.AccountIndex || int(binary.LittleEndian.Uint32(rest[2:6])) != len(rest)-6 {
		return out, bad
	}
	compiled, transaction := rest[6:], ix.Accounts[3:]
	if len(compiled) < 1 || int(compiled[0]) != count {
		return out, bad
	}
	offset := 1
	for range count {
		if offset+2 > len(compiled) {
			return out, bad
		}
		program, accounts := int(compiled[offset]), int(compiled[offset+1])
		offset += 2
		if program >= len(transaction) || offset+accounts+2 > len(compiled) {
			return out, bad
		}
		inner := Instruction{ProgramID: transaction[program].PublicKey}
		for _, index := range compiled[offset : offset+accounts] {
			if int(index) >= len(transaction) {
				return out, bad
			}
			inner.Accounts = append(inner.Accounts, transaction[index])
		}
		offset += accounts
		size := int(binary.LittleEndian.Uint16(compiled[offset:]))
		offset += 2
		if offset+size > len(compiled) {
			return out, bad
		}
		inner.Data = bytes.Clone(compiled[offset : offset+size])
		offset += size
		out.Inner = append(out.Inner, inner)
	}
	if offset != len(compiled) {
		return out, bad
	}
	return out, nil
}
