package backyard

import "fmt"

// catalogEdge is the catalog swap policy a catalog lane swaps action through:
// its conversions, in constraint order, and the leg of the one that converts
// the action's from asset into its to asset.
func catalogEdge(action Action, lane string) ([]conversion, byte, error) {
	if !catalogJupiterRoute(lane) {
		return nil, 0, fmt.Errorf("unregistered Jupiter catalog lane")
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
		return nil, 0, fmt.Errorf("action %s is no catalog conversion on %s", action, lane)
	}
	return catalogSwap(swap.from, swap.to)
}

func catalogConversion(v *Vault, action Action, lane string) (swapEdge, error) {
	conversions, leg, err := catalogEdge(action, lane)
	if err != nil {
		return swapEdge{}, err
	}
	assets, err := routeSwapAssets(v)
	if err != nil {
		return swapEdge{}, err
	}
	c := conversions[leg]
	return swapEdge{assets[c.from], assets[c.to]}, nil
}
