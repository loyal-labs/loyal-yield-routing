package chain

import "github.com/solana-foundation/solana-go/v2"

type LoadedLookupSlot struct {
	Table    solana.PublicKey
	Index    uint8
	Readonly bool
}

// LoadedLookupSlots lists a v0 message's lookup slots in loaded-address order: every table's writable indexes, then every table's readonly indexes.
func LoadedLookupSlots(lookups solana.MessageAddressTableLookupSlice) []LoadedLookupSlot {
	var slots []LoadedLookupSlot
	for _, readonly := range []bool{false, true} {
		for _, lookup := range lookups {
			indexes := lookup.WritableIndexes
			if readonly {
				indexes = lookup.ReadonlyIndexes
			}
			for _, index := range indexes {
				slots = append(slots, LoadedLookupSlot{lookup.AccountKey, index, readonly})
			}
		}
	}
	return slots
}
