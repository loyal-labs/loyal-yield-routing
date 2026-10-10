package backyard

import "fmt"

func catalogJupiterRoute(lane string) bool {
	return lane == "AUTO/AUTO/PYUSD" || lane == "Ethena/USDe/PYUSD" || lane == "Prime/PRIME/PYUSD" || lane == "Prime/PRIME/USDS"
}

// catalogEdge is the catalog swap policy a catalog lane swaps action through:
// its edges, in constraint order, and the leg of the edge that converts the
// action's from asset into its to asset.
func catalogEdge(action Action, lane string) ([2]swapEdge, byte, error) {
	if !catalogJupiterRoute(lane) {
		return [2]swapEdge{}, 0, fmt.Errorf("unregistered Jupiter catalog lane")
	}
	route, err := runtimeRoute(lane)
	if err != nil {
		return [2]swapEdge{}, 0, err
	}
	usdc, collateral, debt := "USDC", route.CollateralSymbol, route.DebtSymbol
	swap, ok := map[Action]conversion{SwapStableToCollateralStep: {usdc, collateral}, SwapCollateralToStableStep: {collateral, usdc},
		SwapDebtToCollateralStep: {debt, collateral}, SwapCollateralToDebtStep: {collateral, debt}, SwapUSDCToDebtStep: {usdc, debt},
		SwapDebtToUSDCStep: {debt, usdc}}[action]
	if !ok {
		return [2]swapEdge{}, 0, fmt.Errorf("action is not a catalog conversion")
	}
	return catalogSwap(swap.from, swap.to)
}
