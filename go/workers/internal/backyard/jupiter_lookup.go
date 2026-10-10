package backyard

import (
	"context"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// Swaps on lanes that take lookup hints support v0 packets. Fresh API table
// identities are encoding hints, never execution authority: chain-owned table
// contents are validated and the compiler only looks up exact keys from the
// policy-validated instruction. No table is created or extended.
func jupiterLookupAddresses(r JupiterSwapRequest) []string {
	if acceptsJupiterLookupHints(r.RouteLane, r.Action) {
		return r.Instruction.LookupTableAddresses
	}
	return nil
}

// basicSwapLaneEdge reports the two approved basic-policy swap directions. Only
// those legs may carry lookup hints; every other basic action keeps dropping
// them.
func basicSwapLaneEdge(action Action) bool {
	switch action {
	case SwapUSDCToPrimeStep, SwapPrimeToUSDCStep, SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep:
		return true
	}
	return false
}

// acceptsJupiterLookupHints: every registry lane keeps the same v0 escape
// hatch for its approved swap edges: hints come only from the fresh quote, are
// resolved from chain, and are pinned to the persisted request identities.
func acceptsJupiterLookupHints(lane string, action Action) bool {
	if !catalogJupiterRoute(lane) {
		// The basic policy lanes swap through the same validated inner
		// instruction wrapped in the same Squads execute. Their oversized legacy
		// packets therefore use the identical v0 escape hatch: hints still come
		// only from the fresh quote, are resolved from chain, and are pinned to
		// the persisted request identities.
		route, err := runtimeRoute(lane)
		return err == nil && route.BasicPolicy && basicSwapLaneEdge(action)
	}
	_, _, err := catalogEdge(action, lane)
	return err == nil
}

func validateJupiterLookupCandidates(addresses []string) error {
	if len(addresses) > 4 {
		return fmt.Errorf("too many Jupiter lookup candidates")
	}
	seen := map[string]bool{}
	for _, address := range addresses {
		key, err := decodeKey(address)
		if err != nil || key == (publicKey{}) || seen[address] {
			return fmt.Errorf("invalid or duplicate Jupiter lookup candidate")
		}
		seen[address] = true
	}
	return nil
}

func validateJupiterLookupIdentities(r JupiterSwapRequest) error {
	if err := validateJupiterLookupCandidates(r.Instruction.LookupTableAddresses); err != nil {
		return err
	}
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

func observeJupiterLookupTables(ctx context.Context, rpc *chain.Client, addresses []string, minimumSlot int64) ([]LookupTableSnapshot, error) {
	if rpc == nil {
		return nil, budgetHold("lookup_observation_unavailable")
	}
	slot, accounts, err := confirmedAccounts(ctx, rpc, addresses, minimumSlot)
	if err != nil {
		return nil, budgetHold("lookup_observation_unavailable")
	}
	tables := make([]LookupTableSnapshot, len(accounts))
	for i, a := range accounts {
		tables[i] = LookupTableSnapshot{Address: a.Address, Owner: a.Owner, Lamports: a.Lamports, Executable: a.Executable, Data: a.Data, ObservedSlot: slot}
		if _, err := decodeMessageLookupTable(tables[i]); err != nil {
			return nil, budgetHold("lookup_account_invalid")
		}
	}
	return tables, nil
}

func (m RouteManifest) prepareJupiterLookupTables(ctx context.Context, rpc *chain.Client, r JupiterSwapRequest, minimumSlot int64) (JupiterSwapRequest, error) {
	if _, err := compileJupiterMessageForDelegate(r, mustKey(bridgeDelegate)); err == nil {
		return r, nil
	}
	addresses := jupiterLookupAddresses(r)
	if err := validateJupiterLookupCandidates(addresses); err != nil {
		return r, err
	}
	if len(addresses) == 0 {
		return r, fmt.Errorf("Jupiter message requires unsupported construction")
	}
	tables, err := observeJupiterLookupTables(ctx, rpc, addresses, minimumSlot)
	if err != nil {
		return r, err
	}
	r.LookupTables = tables
	_, err = compileJupiterMessageForDelegate(r, mustKey(bridgeDelegate))
	return r, err
}

// Called by the common build-cost gate before signer access AND the persisted
// wire's final-send revaluation. Appends are allowed, but can never change the
// persisted message: its entire referenced prefix must still resolve identically.
func revalidateJupiterLookupTables(ctx context.Context, rpc *chain.Client, r JupiterSwapRequest, minimumSlot int64) error {
	if err := validateJupiterLookupIdentities(r); err != nil {
		return budgetHold("lookup_identity_invalid")
	}
	if len(r.LookupTables) == 0 {
		return nil
	}
	for _, s := range r.LookupTables {
		minimumSlot = max(minimumSlot, s.ObservedSlot)
	}
	fresh, err := observeJupiterLookupTables(ctx, rpc, jupiterLookupAddresses(r), minimumSlot)
	if err != nil {
		return err
	}
	for i, retained := range r.LookupTables {
		old, _ := decodeMessageLookupTable(retained)
		now, _ := decodeMessageLookupTable(fresh[i])
		if len(now.addresses) < len(old.addresses) {
			return budgetHold("lookup_mapping_changed")
		}
		for j, key := range old.addresses {
			if key != now.addresses[j] {
				return budgetHold("lookup_mapping_changed")
			}
		}
	}
	return nil
}
