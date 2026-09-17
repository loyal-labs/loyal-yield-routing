package backyardrwa

import (
	"context"
	"fmt"
)

const routeRefreshValuationSource = "unsigned-reserve-refresh-simulation"

// simulateRouteValuationRefresh captures one bank after a closed set of
// permissionless reserve refreshes. Custody, obligation, receipt and policy
// accounts are read-only to these instructions. The simulation fee payer is
// excluded: its virtual fee must never become observed operational spending.
func (c *RPCClient) simulateRouteValuationRefresh(ctx context.Context, route RuntimeRoute, addresses []string, minimumSlot int64) (int64, []ConfirmedAccount, error) {
	for _, address := range addresses {
		if address == bridgeDelegate {
			return 0, nil, fmt.Errorf("valuation capture includes simulated fee payer")
		}
	}
	instructions, err := budgetReserveRefreshInstructions(route.Lane)
	if err != nil {
		return 0, nil, err
	}
	for _, ix := range instructions {
		for i, account := range ix.accounts {
			if account.signer || account.writable && i != 0 {
				return 0, nil, fmt.Errorf("valuation refresh changes non-reserve principal")
			}
		}
	}
	optional := optionalLifecycleObligations(addresses)
	optional = append(optional, bridgeStrategyReceipt)
	slot, accounts, err := c.simulateBudgetReserveRefreshOptional(ctx, route.Lane, addresses, optional, minimumSlot)
	if err != nil {
		return 0, nil, err
	}
	for i := range accounts {
		accounts[i].ValuationSource, accounts[i].ValuationSlot = routeRefreshValuationSource, slot
	}
	if err := validateRouteValuationCapture(slot, accounts, addresses, minimumSlot); err != nil {
		return 0, nil, err
	}
	return slot, accounts, nil
}

func validateRouteValuationCapture(slot int64, accounts []ConfirmedAccount, addresses []string, minimumSlot int64) error {
	if slot < minimumSlot || slot-minimumSlot > budgetMaxObservationLagSlots || len(accounts) != len(addresses) {
		return fmt.Errorf("valuation capture is incomplete or outside freshness window")
	}
	seen := make(map[string]bool, len(addresses))
	for i, account := range accounts {
		if account.Address != addresses[i] || account.Address == bridgeDelegate || seen[account.Address] || account.ValuationSource != routeRefreshValuationSource || account.ValuationSlot != slot {
			return fmt.Errorf("valuation capture namespace or provenance drifted")
		}
		seen[account.Address] = true
	}
	return nil
}

// selectorValuationAddresses retains active valuation/execution inputs and
// every lane's ownership accounts. Unowned lanes' exclusive economic caches
// cannot affect this NAV and need not consume simulation packet space.
func selectorValuationAddresses(route RuntimeRoute, addresses []string) []string {
	protected := map[string]bool{}
	for _, address := range pinnedRouteNAVAddressesForRoute(route) {
		protected[address] = true
	}
	economics := func(r RuntimeRoute) []string {
		return []string{r.Kamino.Market, r.Kamino.CollateralReserve, r.Kamino.DebtReserve, r.CollateralLiquiditySupply, r.DebtLiquiditySupply, r.DebtFeeReceiver}
	}
	for _, address := range economics(route) {
		protected[address] = true
	}
	discard := map[string]bool{}
	for _, lane := range selectorLanes {
		other, err := runtimeRoute(lane)
		if err != nil {
			return addresses
		}
		protected[other.Kamino.Obligation], protected[other.CollateralCustody] = true, true
		if lane != route.Lane {
			for _, address := range economics(other) {
				discard[address] = true
			}
		}
	}
	result := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if !discard[address] || protected[address] {
			result = append(result, address)
		}
	}
	return result
}

// Basic pilot lanes share four policy families. Capture their exact active
// graph plus all bridge policies; legacy lane policy bytes cannot grant this
// route entry or exit authority and need not occupy the refreshed bank.
func selectorValuationPolicyAddresses(m RouteManifest, route RuntimeRoute, addresses []string) []string {
	if !route.BasicPolicy || !selectorLane(route.Lane) {
		return addresses
	}
	required := map[string]bool{}
	for _, family := range []BasicPolicyFamily{BasicCollateralLifecycle, BasicDebtLifecycle, BasicSwapRoutesA, BasicSwapRoutesB} {
		binding, _, err := m.basicPolicyBinding(family)
		if err != nil {
			return addresses
		}
		required[binding.Policy] = true
	}
	for _, binding := range m.RuntimeBindings.BridgePolicies {
		required[binding.Account] = true
	}
	known := map[string]bool{}
	for address := range m.runtimePolicyObservationSet() {
		known[address] = true
	}
	for _, address := range m.PolicyCatalog.PolicyAccounts {
		known[address] = true
	}
	for _, address := range mapleKaminoPolicyAccounts() {
		known[address] = true
	}
	for _, lane := range selectorLanes {
		other, _ := runtimeRoute(lane)
		for _, address := range other.PolicyAccounts {
			known[address] = true
		}
	}
	result := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if !known[address] || required[address] {
			result = append(result, address)
		}
	}
	return result
}
