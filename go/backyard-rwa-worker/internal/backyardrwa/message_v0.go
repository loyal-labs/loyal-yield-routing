package backyardrwa

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

const addressLookupTableProgram = "AddressLookupTab1e1111111111111111111111111"

// Persist the observed table alongside the intent. Recompilation must use this
// exact address prefix, not a newly extended table that could change key order.
type LookupTableSnapshot struct {
	Address      string
	Owner        string
	Lamports     uint64
	Executable   bool
	Data         []byte
	ObservedSlot int64
}

type messageLookupTable struct {
	key       publicKey
	addresses []publicKey
}

func decodeMessageLookupTable(s LookupTableSnapshot) (messageLookupTable, error) {
	key, err := decodeKey(s.Address)
	data := s.Data
	if err != nil || key == (publicKey{}) || s.Owner != addressLookupTableProgram || s.Executable || s.Lamports == 0 ||
		s.ObservedSlot <= 0 || len(data) < 56 || (len(data)-56)%32 != 0 || (len(data)-56)/32 > 256 {
		return messageLookupTable{}, fmt.Errorf("invalid lookup table account")
	}
	// Deactivating tables may still be usable on chain, but are not suitable for
	// preparing a new reserved exit. Newly extended addresses warm up next slot.
	if binary.LittleEndian.Uint32(data[:4]) != 1 || binary.LittleEndian.Uint64(data[4:12]) != math.MaxUint64 ||
		binary.LittleEndian.Uint64(data[12:20]) >= uint64(s.ObservedSlot) || data[21] > 1 || int(data[20]) > (len(data)-56)/32 {
		return messageLookupTable{}, fmt.Errorf("lookup table is inactive, immature or malformed")
	}
	table := messageLookupTable{key: key}
	for i := 56; i < len(data); i += 32 {
		table.addresses = append(table.addresses, publicKeyFromBytes(data[i:i+32]))
	}
	return table, nil
}

// Match the installed web3.js MessageV0 compiler: program-first insertion,
// stable role ordering, table-order writable extraction then readonly extraction,
// and static + all loaded writable + all loaded readonly account indices.
// Only address representation changes; signers and invoked programs stay static.
func compileV0Message(payer, blockhash publicKey, instructions []compiledInstruction, snapshots []LookupTableSnapshot) ([]byte, error) {
	if len(instructions) == 0 || len(instructions) > 255 || len(snapshots) == 0 || len(snapshots) > 256 {
		return nil, fmt.Errorf("invalid versioned message inputs")
	}
	accounts := []accountMeta{{key: payer, signer: true, writable: true}}
	invoked := map[publicKey]bool{}
	for _, ix := range instructions {
		pushOrMergeMeta(&accounts, accountMeta{key: ix.program})
		invoked[ix.program] = true
		for _, a := range ix.accounts {
			pushOrMergeMeta(&accounts, a)
		}
	}
	if len(accounts) > 256 {
		return nil, fmt.Errorf("versioned message exceeds account index space")
	}
	type lookup struct {
		key                publicKey
		writable, readonly []byte
	}
	lookups := []lookup{}
	loadedWritable, loadedReadonly := []accountMeta{}, []accountMeta{}
	loaded, seenTables := map[publicKey]bool{}, map[publicKey]bool{}
	for _, snapshot := range snapshots {
		table, err := decodeMessageLookupTable(snapshot)
		if err != nil {
			return nil, err
		}
		if seenTables[table.key] {
			return nil, fmt.Errorf("duplicate lookup table")
		}
		seenTables[table.key] = true
		l := lookup{key: table.key}
		for _, writable := range []bool{true, false} {
			for _, a := range accounts {
				if a.signer || invoked[a.key] || loaded[a.key] || a.writable != writable {
					continue
				}
				for i, address := range table.addresses {
					if a.key != address {
						continue
					}
					loaded[a.key] = true
					if writable {
						l.writable = append(l.writable, byte(i))
						loadedWritable = append(loadedWritable, a)
					} else {
						l.readonly = append(l.readonly, byte(i))
						loadedReadonly = append(loadedReadonly, a)
					}
					break
				}
			}
		}
		if len(l.writable)+len(l.readonly) > 0 {
			lookups = append(lookups, l)
		}
	}
	static := []accountMeta{}
	for _, a := range accounts {
		if !loaded[a.key] {
			static = append(static, a)
		}
	}
	sort.SliceStable(static, func(i, j int) bool { return accountRank(static[i]) < accountRank(static[j]) })
	if static[0].key != payer {
		return nil, fmt.Errorf("versioned fee payer lost first position")
	}
	var required, readonlySigned, readonlyUnsigned int
	for _, a := range static {
		if a.signer {
			required++
			if !a.writable {
				readonlySigned++
			}
		} else if !a.writable {
			readonlyUnsigned++
		}
	}
	if required > 255 || readonlyUnsigned > 255 {
		return nil, fmt.Errorf("versioned header overflows")
	}
	indices := map[publicKey]byte{}
	all := append(append(append([]accountMeta{}, static...), loadedWritable...), loadedReadonly...)
	for i, a := range all {
		indices[a.key] = byte(i)
	}
	message := []byte{0x80, byte(required), byte(readonlySigned), byte(readonlyUnsigned)}
	message = append(message, encodeShortVec(len(static))...)
	for _, a := range static {
		message = append(message, a.key[:]...)
	}
	message = append(message, blockhash[:]...)
	message = append(message, encodeShortVec(len(instructions))...)
	for _, ix := range instructions {
		message = append(message, indices[ix.program])
		message = append(message, encodeShortVec(len(ix.accounts))...)
		for _, a := range ix.accounts {
			message = append(message, indices[a.key])
		}
		message = append(message, encodeShortVec(len(ix.data))...)
		message = append(message, ix.data...)
	}
	message = append(message, encodeShortVec(len(lookups))...)
	for _, l := range lookups {
		message = append(message, l.key[:]...)
		message = append(message, encodeShortVec(len(l.writable))...)
		message = append(message, l.writable...)
		message = append(message, encodeShortVec(len(l.readonly))...)
		message = append(message, l.readonly...)
	}
	return message, nil
}
