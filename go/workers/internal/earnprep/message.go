package earnprep

import (
	"errors"
	"slices"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

type LookupTable struct {
	Key       solana.PublicKey
	Addresses solana.PublicKeySlice
}

type keyMeta struct {
	key                       solana.PublicKey
	signer, writable, invoked bool
}

func CompileV0(payer solana.PublicKey, blockhash solana.Hash, instructions []squads.Instruction, tables []LookupTable) (*solana.Transaction, error) {
	var keys []keyMeta
	meta := func(key solana.PublicKey) *keyMeta {
		for i := range keys {
			if keys[i].key == key {
				return &keys[i]
			}
		}
		keys = append(keys, keyMeta{key: key})
		return &keys[len(keys)-1]
	}
	p := meta(payer)
	p.signer, p.writable = true, true
	for _, ix := range instructions {
		meta(ix.ProgramID).invoked = true
		for _, account := range ix.Accounts {
			m := meta(account.PublicKey)
			m.signer = m.signer || account.IsSigner
			m.writable = m.writable || account.IsWritable
		}
	}
	var lookups solana.MessageAddressTableLookupSlice
	var loadedWritable, loadedReadonly []solana.PublicKey
	for _, table := range tables {
		drain := func(writable bool) ([]uint8, []solana.PublicKey) {
			var indexes []uint8
			var drained []solana.PublicKey
			keys = slices.DeleteFunc(keys, func(m keyMeta) bool {
				if m.signer || m.invoked || m.writable != writable {
					return false
				}
				at := slices.Index(table.Addresses, m.key)
				if at < 0 || at > 255 {
					return false
				}
				indexes = append(indexes, uint8(at))
				drained = append(drained, m.key)
				return true
			})
			return indexes, drained
		}
		writableIndexes, writableKeys := drain(true)
		readonlyIndexes, readonlyKeys := drain(false)
		if len(writableIndexes)+len(readonlyIndexes) == 0 {
			continue
		}
		lookups = append(lookups, solana.MessageAddressTableLookup{AccountKey: table.Key, WritableIndexes: writableIndexes, ReadonlyIndexes: readonlyIndexes})
		loadedWritable = append(loadedWritable, writableKeys...)
		loadedReadonly = append(loadedReadonly, readonlyKeys...)
	}
	group := func(signer, writable bool) []solana.PublicKey {
		var out []solana.PublicKey
		for _, m := range keys {
			if m.signer == signer && m.writable == writable {
				out = append(out, m.key)
			}
		}
		return out
	}
	writableSigners, readonlySigners := group(true, true), group(true, false)
	writableUnsigned, readonlyUnsigned := group(false, true), group(false, false)
	static := slices.Concat(writableSigners, readonlySigners, writableUnsigned, readonlyUnsigned)
	if len(static) > 256 {
		return nil, errors.New("earnprep: message exceeds 256 static account keys")
	}
	all := slices.Concat(static, loadedWritable, loadedReadonly)
	index := func(key solana.PublicKey) uint16 { return uint16(slices.Index(all, key)) }
	compiled := make([]solana.CompiledInstruction, len(instructions))
	for i, ix := range instructions {
		accounts := make([]uint16, len(ix.Accounts))
		for j, account := range ix.Accounts {
			accounts[j] = index(account.PublicKey)
		}
		compiled[i] = solana.CompiledInstruction{ProgramIDIndex: index(ix.ProgramID), Accounts: accounts, Data: ix.Data}
	}
	message := solana.Message{
		AccountKeys: static,
		Header: solana.MessageHeader{
			NumRequiredSignatures:       uint8(len(writableSigners) + len(readonlySigners)),
			NumReadonlySignedAccounts:   uint8(len(readonlySigners)),
			NumReadonlyUnsignedAccounts: uint8(len(readonlyUnsigned)),
		},
		RecentBlockhash:     blockhash,
		Instructions:        compiled,
		AddressTableLookups: lookups,
	}
	message.SetVersion(solana.MessageVersionV0)
	return &solana.Transaction{Signatures: make([]solana.Signature, message.Header.NumRequiredSignatures), Message: message}, nil
}
