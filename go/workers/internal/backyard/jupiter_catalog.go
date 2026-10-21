package backyard

import "fmt"

func catalogEdge(action Action, lane string) (catalogEdgePolicy, error) {
	if !catalogJupiterRoute(lane) {
		return catalogEdgePolicy{}, fmt.Errorf("unregistered Jupiter catalog lane")
	}
	l, _ := findEarnLane(lane)
	route := l.route
	usdc, collateral, debt := "USDC", route.CollateralSymbol, route.DebtSymbol
	swaps := map[Action]conversion{SwapStableToCollateralStep: {usdc, collateral}, SwapCollateralToStableStep: {collateral, usdc},
		SwapDebtToCollateralStep: {debt, collateral}, SwapCollateralToDebtStep: {collateral, debt}, SwapUSDCToDebtStep: {usdc, debt},
		SwapDebtToUSDCStep: {debt, usdc}}
	if route.Lane == autoAUTOPYUSDTemplate.Lane {
		delete(swaps, SwapUSDCToDebtStep)
	}
	if hastraLane(route) {
		for action := range swaps {
			if _, hastra := hastraConversionLeg(action, lane); hastra {
				delete(swaps, action)
			}
		}
		delete(swaps, SwapDebtToUSDCStep)
	}
	swap, ok := swaps[action]
	if !ok {
		return catalogEdgePolicy{}, fmt.Errorf("action %s is no catalog conversion on %s", action, lane)
	}
	return catalogSwap(swap.from, swap.to)
}

func catalogConversion(v *Vault, action Action, lane string) (swapEdge, error) {
	edge, err := catalogEdge(action, lane)
	if err != nil {
		return swapEdge{}, err
	}
	assets, err := routeSwapAssets(v)
	if err != nil {
		return swapEdge{}, err
	}
	c := edge.conversion()
	return swapEdge{assets[c.from], assets[c.to]}, nil
}
