package backyardrwa

import (
	"context"
	"fmt"
)

// Existing tables referenced by the retained, installed-catalog USDe->PYUSD
// instruction. No table creation, extension, authority change or API-supplied
// identity is authorized by this list. Resolve public accounts afresh at runtime.
func jupiterLookupAddresses(r JupiterSwapRequest) []string {
	if r.RouteLane == "Ethena/USDe/PYUSD" && r.Action == SwapCollateralToDebtStep {
		return []string{"FQCY2Cbea1jazkUc6xjBUD72gMT2o8Mr4mnMd7gpL2F1", "8mLN3ZeRSmrMuRZf3CcWfcUF19FaCLJVRNQastGkdh4M"}
	}
	return nil
}

func validateJupiterLookupIdentities(r JupiterSwapRequest) error {
	if len(r.LookupTables) == 0 {
		return nil
	}
	addresses := jupiterLookupAddresses(r)
	if len(addresses) != len(r.LookupTables) {
		return fmt.Errorf("unreviewed Jupiter lookup set")
	}
	for i, s := range r.LookupTables {
		if s.Address != addresses[i] {
			return fmt.Errorf("unreviewed Jupiter lookup identity or order")
		}
		if _, err := decodeMessageLookupTable(s); err != nil {
			return err
		}
	}
	return nil
}

func observeJupiterLookupTables(ctx context.Context, rpc *RPCClient, addresses []string, minimumSlot int64) ([]LookupTableSnapshot, int64, error) {
	if rpc == nil {
		return nil, 0, budgetHold("lookup_observation_unavailable")
	}
	slot, accounts, err := rpc.GetMultipleAccounts(ctx, addresses, minimumSlot)
	if err != nil {
		return nil, 0, budgetHold("lookup_observation_unavailable")
	}
	tables := make([]LookupTableSnapshot, len(accounts))
	for i, a := range accounts {
		tables[i] = LookupTableSnapshot{Address: a.Address, Owner: a.Owner, Lamports: a.Lamports, Executable: a.Executable, Data: a.Data, ObservedSlot: slot}
		if _, err := decodeMessageLookupTable(tables[i]); err != nil {
			return nil, 0, budgetHold("lookup_account_invalid")
		}
	}
	return tables, slot, nil
}

func prepareJupiterLookupTables(ctx context.Context, rpc *RPCClient, r JupiterSwapRequest, minimumSlot int64) (JupiterSwapRequest, error) {
	if _, err := CompileJupiterMessage(r); err == nil {
		return r, nil
	}
	addresses := jupiterLookupAddresses(r)
	if len(addresses) == 0 {
		return r, fmt.Errorf("Jupiter message requires unsupported construction")
	}
	tables, _, err := observeJupiterLookupTables(ctx, rpc, addresses, minimumSlot)
	if err != nil {
		return r, err
	}
	r.LookupTables = tables
	_, err = CompileJupiterMessage(r)
	return r, err
}

// Called by the common build-cost gate before signer access AND the persisted
// wire's final-send revaluation. Appends are allowed, but can never change the
// persisted message: its entire referenced prefix must still resolve identically.
func revalidateJupiterLookupTables(ctx context.Context, rpc *RPCClient, r JupiterSwapRequest, minimumSlot int64) (int64, error) {
	if err := validateJupiterLookupIdentities(r); err != nil {
		return 0, budgetHold("lookup_identity_invalid")
	}
	if len(r.LookupTables) == 0 {
		return minimumSlot, nil
	}
	for _, s := range r.LookupTables {
		minimumSlot = max(minimumSlot, s.ObservedSlot)
	}
	fresh, slot, err := observeJupiterLookupTables(ctx, rpc, jupiterLookupAddresses(r), minimumSlot)
	if err != nil {
		return 0, err
	}
	for i, retained := range r.LookupTables {
		old, _ := decodeMessageLookupTable(retained)
		now, _ := decodeMessageLookupTable(fresh[i])
		if len(now.addresses) < len(old.addresses) {
			return 0, budgetHold("lookup_mapping_changed")
		}
		for j, key := range old.addresses {
			if key != now.addresses[j] {
				return 0, budgetHold("lookup_mapping_changed")
			}
		}
	}
	return slot, nil
}
