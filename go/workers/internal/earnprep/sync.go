package earnprep

import (
	"encoding/binary"
	"errors"
	"math"
	"slices"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

const (
	syncPayloadTransaction = 0

	MaxComputeUnitLimit      = 1_400_000
	SyncExecuteUnitsHeadroom = 60_000
)

type MemberSync struct {
	Settings     solana.PublicKey
	Vault        solana.PublicKey
	AccountIndex uint8
	Members      []solana.PublicKey
	Inner        []squads.Instruction
}

func SyncExecuteUnits(units uint32) uint32 {
	return min(MaxComputeUnitLimit, units+SyncExecuteUnitsHeadroom)
}

func ExecuteMemberSync(s MemberSync) (squads.Instruction, error) {
	if len(s.Inner) == 0 || len(s.Inner) > math.MaxUint8 || len(s.Members) == 0 || len(s.Members) > math.MaxUint8 {
		return squads.Instruction{}, errors.New("earnprep: a member sync execute needs 1-255 inner instructions and members")
	}
	tail, err := syncTail(s.Vault, s.Inner)
	if err != nil {
		return squads.Instruction{}, err
	}
	compiled := []byte{uint8(len(s.Inner))}
	for _, ix := range s.Inner {
		if len(ix.Accounts) > math.MaxUint8 || len(ix.Data) > math.MaxUint16 {
			return squads.Instruction{}, errors.New("earnprep: sync inner instruction overflows its compact encoding")
		}
		compiled = append(compiled, tailIndex(tail, ix.ProgramID), uint8(len(ix.Accounts)))
		for _, meta := range ix.Accounts {
			compiled = append(compiled, tailIndex(tail, meta.PublicKey))
		}
		compiled = binary.LittleEndian.AppendUint16(compiled, uint16(len(ix.Data)))
		compiled = append(compiled, ix.Data...)
	}
	data := append([]byte(nil), squads.ExecuteTransactionSyncV2Discriminator[:]...)
	data = append(data, s.AccountIndex, uint8(len(s.Members)), syncPayloadTransaction)
	data = binary.LittleEndian.AppendUint32(data, uint32(len(compiled)))
	data = append(data, compiled...)
	accounts := make([]solana.AccountMeta, 0, 2+len(s.Members)+len(tail))
	accounts = append(accounts, solana.AccountMeta{PublicKey: s.Settings, IsWritable: true}, solana.AccountMeta{PublicKey: squads.ProgramID})
	for _, member := range slices.Backward(s.Members) {
		accounts = append(accounts, solana.AccountMeta{PublicKey: member, IsSigner: true})
	}
	return squads.Instruction{ProgramID: squads.ProgramID, Accounts: append(accounts, tail...), Data: data}, nil
}

func syncTail(vault solana.PublicKey, inner []squads.Instruction) ([]solana.AccountMeta, error) {
	var keys []solana.AccountMeta
	meta := func(key solana.PublicKey) *solana.AccountMeta {
		for i := range keys {
			if keys[i].PublicKey == key {
				return &keys[i]
			}
		}
		keys = append(keys, solana.AccountMeta{PublicKey: key})
		return &keys[len(keys)-1]
	}
	for _, ix := range inner {
		meta(ix.ProgramID)
		for _, account := range ix.Accounts {
			m := meta(account.PublicKey)
			m.IsSigner = m.IsSigner || account.IsSigner
			m.IsWritable = m.IsWritable || account.IsWritable
		}
	}
	if len(keys) > 256 {
		return nil, errors.New("earnprep: sync transaction exceeds 256 accounts")
	}
	rank := func(m solana.AccountMeta) int {
		switch {
		case m.IsSigner && m.IsWritable:
			return 0
		case m.IsSigner:
			return 1
		case m.IsWritable:
			return 2
		}
		return 3
	}
	slices.SortStableFunc(keys, func(a, b solana.AccountMeta) int { return rank(a) - rank(b) })
	for i := range keys {
		keys[i].IsSigner = keys[i].IsSigner && keys[i].PublicKey != vault
	}
	return keys, nil
}

func tailIndex(tail []solana.AccountMeta, key solana.PublicKey) uint8 {
	return uint8(slices.IndexFunc(tail, func(m solana.AccountMeta) bool { return m.PublicKey == key }))
}
