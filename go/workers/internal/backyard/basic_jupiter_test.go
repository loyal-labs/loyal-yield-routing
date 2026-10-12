package backyard

import (
	"testing"
)

func basicSwapActionForLeg(leg string) Action {
	switch leg {
	case "USDC->PRIME", "USDC->syrupUSDC", "USDC->ONyc":
		return SwapStableToCollateralStep
	case "PRIME->USDC", "syrupUSDC->USDC", "ONyc->USDC":
		return SwapCollateralToStableStep
	}
	return ""
}

// basicJupiterRequest is a basic lane's swap of leg as the production builder
// makes it from the v2 fixture.
func basicJupiterRequest(t *testing.T, lane, leg string) JupiterSwapRequest {
	t.Helper()
	request := fixtureSwapRequest(t, lane, basicSwapActionForLeg(leg))
	compileTestSwap(t, request)
	return request
}
