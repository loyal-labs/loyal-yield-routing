package autodeposit

import (
	"context"
	"encoding/binary"
	"errors"

	"github.com/solana-foundation/solana-go/v2"
)

func (b *SweepWireBuilder) resolvePersistedLookups(ctx context.Context, tx *solana.Transaction) error {
	lookups := tx.Message.GetAddressTableLookups()
	if len(lookups) == 0 {
		return nil
	}
	addresses := make([]string, 0, len(lookups))
	seen := map[string]bool{}
	for _, lookup := range lookups {
		address := lookup.AccountKey.String()
		if seen[address] {
			return errors.New("persisted packet repeats lookup table")
		}
		seen[address] = true
		addresses = append(addresses, address)
	}
	slot, accounts, err := b.read(ctx, 0, addresses)
	if err != nil {
		return err
	}
	if slot <= 0 || len(accounts) != len(addresses) {
		return errors.New("persisted lookup evidence incomplete")
	}
	tables := map[solana.PublicKey]solana.PublicKeySlice{}
	for _, a := range accounts {
		// Solana AddressLookupTable bincode metadata occupies 56 bytes. Table
		// entries are append-only; deactivated/just-extended tables fail closed.
		if a.Owner != solana.AddressLookupTableProgramID || a.Executable || len(a.Data) < 56 || (len(a.Data)-56)%32 != 0 || binary.LittleEndian.Uint32(a.Data[:4]) != 1 || binary.LittleEndian.Uint64(a.Data[4:12]) != ^uint64(0) || binary.LittleEndian.Uint64(a.Data[12:20]) >= uint64(slot) {
			return errors.New("persisted lookup table is unavailable, deactivated or not yet usable")
		}
		keys := make(solana.PublicKeySlice, 0, (len(a.Data)-56)/32)
		if cap(keys) > 256 {
			return errors.New("lookup table exceeds address bound")
		}
		for offset := 56; offset < len(a.Data); offset += 32 {
			keys = append(keys, solana.PublicKeyFromBytes(a.Data[offset:offset+32]))
		}
		tables[a.Key] = keys
	}
	if err = tx.Message.SetAddressTables(tables); err != nil {
		return err
	}
	return tx.Message.ResolveLookups()
}
