package backyard

import "fmt"

// catalogEdge is the catalog swap policy a catalog lane swaps action through:
// its edges, in constraint order, and the leg of the edge that converts the
// action's from asset into its to asset.
func catalogEdge(action Action, lane string) ([]swapEdge, byte, error) {
	if !catalogJupiterRoute(lane) {
		return nil, 0, fmt.Errorf("unregistered Jupiter catalog lane")
	}
	route, err := runtimeRoute(lane)
	if err != nil {
		return nil, 0, err
	}
	usdc, collateral, debt := "USDC", route.CollateralSymbol, route.DebtSymbol
	swaps := map[Action]conversion{SwapStableToCollateralStep: {usdc, collateral}, SwapCollateralToStableStep: {collateral, usdc},
		SwapDebtToCollateralStep: {debt, collateral}, SwapCollateralToDebtStep: {collateral, debt}, SwapUSDCToDebtStep: {usdc, debt},
		SwapDebtToUSDCStep: {debt, usdc}}
	if route.Lane == autoAUTOPYUSD.Lane {
		// AUTO's recipe has no USDC into its debt: it repays from collateral.
		delete(swaps, SwapUSDCToDebtStep)
	}
	swap, ok := swaps[action]
	if !ok {
		return nil, 0, fmt.Errorf("action %s is no catalog conversion on %s", action, lane)
	}
	return catalogSwap(swap.from, swap.to)
}

// catalogConversion is the edge a catalog lane swaps action along.
func catalogConversion(action Action, lane string) (swapEdge, error) {
	edges, leg, err := catalogEdge(action, lane)
	if err != nil {
		return swapEdge{}, err
	}
	return edges[leg], nil
}
