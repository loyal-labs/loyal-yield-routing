package fleetexec

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	sdk "github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// The lookup family reads finalized state only. A table and the SlotHashes
// sysvar come from one call, so the cooldown proof and the table it gates
// describe the same bank.

func lookupSnapshot(ctx context.Context, c *chain.Client, address string, minSlot int64) (LookupSnapshot, error) {
	key, err := sdk.PublicKeyFromBase58(address)
	if err != nil || minSlot < 0 {
		return LookupSnapshot{}, errors.New("invalid lookup readback request")
	}
	slot, accounts, err := c.Accounts(ctx, []sdk.PublicKey{key, sdk.SysVarSlotHashesPubkey}, rpc.CommitmentFinalized, uint64(minSlot))
	if err != nil {
		return LookupSnapshot{}, err
	}
	return decodeLookupSnapshot(address, int64(slot), accounts[0], accounts[1])
}

func decodeLookupSnapshot(address string, slot int64, a, hashes *chain.Account) (LookupSnapshot, error) {
	out := LookupSnapshot{Address: address, Slot: slot}
	if slot <= 0 {
		return out, errors.New("lookup account finalized context is missing")
	}
	if hashes == nil || hashes.Executable || hashes.Owner != sdk.SysVarPubkey || len(hashes.Data) < 8 {
		return out, errors.New("lookup SlotHashes owner/data mismatch")
	}
	hd := hashes.Data
	count := binary.LittleEndian.Uint64(hd[:8])
	if count == 0 || count > 512 || (uint64(len(hd)) != 8+40*count && len(hd) != 8+40*512) {
		return out, errors.New("lookup SlotHashes is incomplete")
	}
	for _, padding := range hd[8+40*count:] {
		if padding != 0 {
			return out, errors.New("lookup SlotHashes padding is noncanonical")
		}
	}
	for i := uint64(0); i < count; i++ {
		s := binary.LittleEndian.Uint64(hd[8+40*i:])
		if s > uint64(slot) || (i > 0 && s >= out.SlotHashes[i-1]) {
			return out, errors.New("lookup SlotHashes ordering/frontier mismatch")
		}
		out.SlotHashes = append(out.SlotHashes, s)
	}
	if a == nil {
		out.Absent = true
		return out, nil
	}
	d := a.Data
	if a.Executable || a.Owner.String() != lookupProgram || len(d) < 56 || (len(d)-56)%32 != 0 || (len(d)-56)/32 > 256 || binary.LittleEndian.Uint32(d[:4]) != 1 || d[21] > 1 {
		return out, errors.New("lookup table owner/state/length mismatch")
	}
	out.Owner, out.Lamports, out.Data = lookupProgram, a.Lamports, d
	out.DeactivationSlot = binary.LittleEndian.Uint64(d[4:12])
	out.LastExtendedSlot = binary.LittleEndian.Uint64(d[12:20])
	out.LastExtendedStartIndex = d[20]
	if out.LastExtendedSlot > uint64(slot) || (out.DeactivationSlot != ^uint64(0) && out.DeactivationSlot > uint64(slot)) {
		return out, errors.New("lookup table metadata is newer than finalized context")
	}
	if d[21] == 1 {
		out.Authority = sdk.PublicKeyFromBytes(d[22:54]).String()
	}
	for offset := 56; offset < len(d); offset += 32 {
		out.Addresses = append(out.Addresses, sdk.PublicKeyFromBytes(d[offset:offset+32]).String())
	}
	if int(out.LastExtendedStartIndex) > len(out.Addresses) {
		return out, errors.New("lookup extension start exceeds membership")
	}
	return out, nil
}

// Catalog drift may observe an absent or differently owned account. Financial
// recovery still uses lookupSnapshot's strict program-owner decoder.
type lookupCatalogObservation struct {
	snapshot LookupSnapshot
	reason   string
}

func lookupCatalogObservations(ctx context.Context, c *chain.Client, addresses []string, minSlot int64) ([]lookupCatalogObservation, error) {
	if len(addresses) == 0 || len(addresses) > 16 || minSlot < 0 || len(lookupAddressSet(addresses)) != len(addresses) {
		return nil, errors.New("invalid bounded catalog observation request")
	}
	keys := make([]sdk.PublicKey, 0, len(addresses)+1)
	for _, address := range addresses {
		key, err := sdk.PublicKeyFromBase58(address)
		if err != nil {
			return nil, errors.New("invalid catalog account address")
		}
		keys = append(keys, key)
	}
	slot, accounts, err := c.Accounts(ctx, append(keys, sdk.SysVarSlotHashesPubkey), rpc.CommitmentFinalized, uint64(minSlot))
	if err != nil {
		return nil, err
	}
	hashes := accounts[len(addresses)]
	var observations []lookupCatalogObservation
	for n, address := range addresses {
		base, err := decodeLookupSnapshot(address, int64(slot), nil, hashes)
		if err != nil {
			return nil, err
		}
		account := accounts[n]
		if account == nil {
			observations = append(observations, lookupCatalogObservation{snapshot: base, reason: "finalized_shared_table_missing"})
			continue
		}
		if account.Executable {
			return nil, errors.New("incomplete lookup account evidence")
		}
		base.Absent, base.Owner, base.Data, base.Lamports = false, account.Owner.String(), account.Data, account.Lamports
		if base.Owner != lookupProgram {
			observations = append(observations, lookupCatalogObservation{snapshot: base, reason: "finalized_shared_table_owner_drift"})
			continue
		}
		decoded, err := decodeLookupSnapshot(address, int64(slot), account, hashes)
		if err != nil {
			observations = append(observations, lookupCatalogObservation{snapshot: base, reason: "finalized_shared_table_decode_drift"})
			continue
		}
		observations = append(observations, lookupCatalogObservation{snapshot: decoded})
	}
	return observations, nil
}

// lookupFinalizedReceipt is nil while finalized history has no record of sig.
func lookupFinalizedReceipt(ctx context.Context, c *chain.Client, sig string) (*LookupReceipt, error) {
	signature, err := sdk.SignatureFromBase58(sig)
	if err != nil {
		return nil, errors.New("invalid lookup receipt signature")
	}
	receipt, err := c.Receipt(ctx, signature, rpc.CommitmentFinalized)
	if errors.Is(err, chain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	tx, err := sdk.TransactionFromBytes(receipt.Wire)
	if err != nil || tx.Message.IsVersioned() || len(tx.Signatures) != 1 || tx.Signatures[0] != signature {
		return nil, errors.New("lookup receipt packet signature/version mismatch")
	}
	out := &LookupReceipt{Signature: sig, Slot: int64(receipt.Slot), Wire: receipt.Wire, FeeLamports: receipt.Fee, PreLamports: receipt.PreLamports, PostLamports: receipt.PostLamports}
	for _, key := range receipt.Keys {
		out.Addresses = append(out.Addresses, key.String())
	}
	if len(out.PreLamports) != len(out.Addresses) || len(out.PostLamports) != len(out.Addresses) {
		return nil, errors.New("lookup receipt balance indices incomplete")
	}
	if receipt.Err != nil {
		text, err := json.Marshal(receipt.Err)
		if err != nil {
			return nil, errors.New("lookup receipt error is not JSON")
		}
		out.Err = string(text)
	}
	return out, nil
}

// lookupBlockhash is a finalized blockhash, its expiry height and the bank
// slot it was read at.
func lookupBlockhash(ctx context.Context, c *chain.Client) (string, int64, int64, error) {
	hash, height, slot, err := c.Blockhash(ctx, rpc.CommitmentFinalized, 0)
	return hash.String(), int64(height), int64(slot), err
}

// lookupBalance is key's finalized lamports at or after minSlot; an absent
// account holds none.
func lookupBalance(ctx context.Context, c *chain.Client, key string, minSlot int64) (uint64, error) {
	pub, err := sdk.PublicKeyFromBase58(key)
	if err != nil || minSlot < 0 {
		return 0, errors.New("invalid lookup balance request")
	}
	_, accounts, err := c.Accounts(ctx, []sdk.PublicKey{pub}, rpc.CommitmentFinalized, uint64(minSlot))
	if err != nil || accounts[0] == nil {
		return 0, err
	}
	return accounts[0].Lamports, nil
}
