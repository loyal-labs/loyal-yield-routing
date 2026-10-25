package earnprep

import (
	"errors"
	"fmt"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

type KaminoEarnReserveExpectation struct {
	LendProgramID         solana.PublicKey
	Market                solana.PublicKey
	LiquidityMint         solana.PublicKey
	LiquidityTokenProgram solana.PublicKey
}

// ValidateKaminoEarnReserveAccount ports validateKaminoEarnReserveAccount
// (CL:1141-1182) over kamino.DecodeReserve: the reserve account must be owned
// by the lending program and carry the target's market, liquidity mint and
// liquidity token program; the decoded reserve is returned for the farm and
// collateral checks downstream (CL:7875-7966). Only mainnet runs it
// (CL:7819-7822 skips devnet); account is the getAccountInfo result, whose
// absence the caller reports (CL:7803-7805). Error strings are the TS's,
// they surface in the route's prepare_failed body.
func ValidateKaminoEarnReserveAccount(account *chain.Account, target KaminoEarnReserveExpectation) (kamino.Reserve, error) {
	if account == nil {
		return kamino.Reserve{}, errors.New("Selected Kamino reserve account was not found.")
	}
	if account.Owner != target.LendProgramID {
		return kamino.Reserve{}, errors.New("Selected Kamino reserve is not owned by the selected lending program.")
	}
	reserve, err := kamino.DecodeReserve(account)
	if err != nil {
		return kamino.Reserve{}, err
	}
	for _, check := range []struct {
		actual, expected solana.PublicKey
		label            string
	}{
		{actual: reserve.LendingMarket, expected: target.Market, label: "lending market"},
		{actual: reserve.LiquidityMint, expected: target.LiquidityMint, label: "liquidity mint"},
		{actual: reserve.LiquidityTokenProgram, expected: target.LiquidityTokenProgram, label: "liquidity token program"},
	} {
		if check.actual != check.expected {
			return kamino.Reserve{}, fmt.Errorf("Selected Kamino reserve has an unexpected %s.", check.label)
		}
	}
	return reserve, nil
}
