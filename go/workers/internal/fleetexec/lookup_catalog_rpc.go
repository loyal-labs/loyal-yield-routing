package fleetexec

import (
	"context"
	"errors"

	sdk "github.com/gagliardetto/solana-go"
)

// Catalog drift may observe an absent or differently owned account. Financial
// recovery still uses LookupSnapshot's strict program-owner decoder.
type lookupCatalogObservation struct {
	snapshot LookupSnapshot
	reason   string
}

func (r *LookupRPC) lookupCatalogObservations(ctx context.Context, addresses []string, minSlot int64) ([]lookupCatalogObservation, error) {
	var result struct {
		Context struct {
			Slot int64 `json:"slot"`
		} `json:"context"`
		Value []*lookupRPCAccount `json:"value"`
	}
	if len(addresses) == 0 || len(addresses) > 16 || minSlot < 0 || len(lookupAddressSet(addresses)) != len(addresses) {
		return nil, errors.New("invalid bounded catalog observation request")
	}
	for _, address := range addresses {
		if _, err := sdk.PublicKeyFromBase58(address); err != nil {
			return nil, errors.New("invalid catalog account address")
		}
	}
	keys := append(append([]string{}, addresses...), sdk.SysVarSlotHashesPubkey.String())
	if err := r.call(ctx, &result, "getMultipleAccounts", keys, map[string]any{"commitment": "finalized", "encoding": "base64", "minContextSlot": minSlot}); err != nil {
		return nil, err
	}
	if len(result.Value) != len(keys) || result.Context.Slot < minSlot {
		return nil, errors.New("catalog finalized readback is incomplete or stale")
	}
	var observations []lookupCatalogObservation
	for n, address := range addresses {
		base, err := decodeLookupSnapshot(address, result.Context.Slot, nil, result.Value[len(addresses)])
		if err != nil {
			return nil, err
		}
		account := result.Value[n]
		if account == nil {
			observations = append(observations, lookupCatalogObservation{snapshot: base, reason: "finalized_shared_table_missing"})
			continue
		}
		data, err := lookupAccountBytes(account)
		if err != nil {
			return nil, err
		}
		base.Absent = false
		base.Owner = account.Owner
		base.Data = data
		base.Lamports = *account.Lamports
		if account.Owner != lookupProgram {
			observations = append(observations, lookupCatalogObservation{snapshot: base, reason: "finalized_shared_table_owner_drift"})
			continue
		}
		decoded, err := decodeLookupSnapshot(address, result.Context.Slot, account, result.Value[len(addresses)])
		if err != nil {
			observations = append(observations, lookupCatalogObservation{snapshot: base, reason: "finalized_shared_table_decode_drift"})
			continue
		}
		observations = append(observations, lookupCatalogObservation{snapshot: decoded})
	}
	return observations, nil
}
