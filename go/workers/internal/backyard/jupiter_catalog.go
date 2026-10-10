package backyard

import (
	"encoding/base64"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

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

// validateCatalogJupiterInstruction checks what the worker asked of a catalog
// swap: the instruction's in_amount is the request's and its quoted out the
// quote's, and the minimum it keeps is at most that quote. It reads them where
// the edge's route tail is, so the data must end with that tail: the policy
// bounds the bytes at those offsets but cannot bound the data's length. The
// policy and Jupiter check the rest.
func validateCatalogJupiterInstruction(value JupiterSwapInstruction, action Action, amount, out, minimum uint64, lane string) (compiledInstruction, error) {
	edges, leg, err := catalogEdge(action, lane)
	if err != nil {
		return compiledInstruction{}, err
	}
	edge := edges[leg]
	data, err := base64.StdEncoding.Strict().DecodeString(value.Data)
	if err != nil || len(data) != edge.feeAt()+1 || amount == 0 || minimum == 0 || minimum > out ||
		readU64(data[edge.amountAt():]) != amount || readU64(data[edge.quotedAt():]) != out {
		return compiledInstruction{}, fmt.Errorf("Jupiter instruction does not swap the requested amount at the quoted output")
	}
	accounts := make([]accountMeta, len(value.Accounts))
	for i, input := range value.Accounts {
		key, err := decodeKey(input.Pubkey)
		if err != nil {
			return compiledInstruction{}, err
		}
		accounts[i] = accountMeta{key: key, signer: input.IsSigner, writable: input.IsWritable}
	}
	return compiledInstruction{program: publicKey(jupiter.ProgramID), accounts: accounts, data: data}, nil
}
