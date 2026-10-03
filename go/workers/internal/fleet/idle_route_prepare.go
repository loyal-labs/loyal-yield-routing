package fleet

import "fmt"

// PrepareIdleDepositRoute compiles and simulates a single Squads-protected
// deposit. It does not sign, publish, reserve capacity or acquire idle custody.
// Missing target setup and durable revalidation remain separate requirements.
func PrepareIdleDepositRoute(route KaminoSameMintRoute, request KaminoIdleDepositRequest, policy, signer string, policyAccountIndex, allowedIndex uint8, tables []LookupTable, recentBlockhash string, feeLamports, computeLimit uint64, simulate func([]byte) (SimulationEvidence, error)) (RoutePreparation, error) {
	if allowedIndex != 1 {
		return RoutePreparation{}, fmt.Errorf("idle deposit must use retained deposit policy constraint 1")
	}
	if err := validateIdleProxyRoute(route, request); err != nil {
		return RoutePreparation{}, err
	}
	if computeLimit == 0 || computeLimit > defaultComputeLimit {
		return RoutePreparation{}, fmt.Errorf("invalid idle compute limit")
	}
	route.Public = append(computeBudgetInstructions(uint32(computeLimit), 0), route.Public...)
	layout := func(public, wrapped []RouteInstruction) ([]RouteInstruction, error) {
		if len(public) != 4 || len(wrapped) != 1 {
			return nil, fmt.Errorf("idle route must have a compute budget, two refreshes and one protected deposit")
		}
		result := append([]RouteInstruction(nil), public...)
		return append(result, wrapped[0]), nil
	}
	return prepareRoute(route, policy, signer, policyAccountIndex, []uint8{allowedIndex}, tables, recentBlockhash, feeLamports, computeLimit, simulate, layout, "idle_vault_deposit_kamino_v0")
}
