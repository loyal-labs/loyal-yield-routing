package backyard

import (
	"context"
	"errors"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// Read-only price reference discovered at confirmed slot 444381476 by exact
// program, market and mint filters. This is NOT an executable runtime lane.
const budgetSOLReserve = "d4A2prbA2whesmvHaL88BH6Ewn5N4bTSU2Ze8P6Bc4Q"
const budgetSOLMarket = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"

var budgetWrappedSOLMint = solana.WrappedSol.String()

func ObserveNativeSOLBudgetPrice(ctx context.Context, rpc *chain.Client, minimumSlot int64) (BudgetPrice, error) {
	if rpc == nil || minimumSlot <= 0 {
		return BudgetPrice{}, budgetHold("invalid_price_observation_request")
	}
	reference, err := pinnedKaminoObservationConfig()
	if err != nil {
		return BudgetPrice{}, err
	}
	config := KaminoObservationConfig{Program: kamino.ProgramID.String(), Market: budgetSOLMarket}
	debit := ExecutableDebit{Mint: budgetWrappedSOLMint, TokenProgram: classicTokenProgram, Raw: 1}
	addresses := []string{budgetSOLReserve, reference.DebtReserve, budgetWrappedSOLMint, bridgeUSDC, budgetClockAddress}
	slot, accounts, err := confirmedAccounts(ctx, rpc, addresses, minimumSlot)
	if err != nil {
		return BudgetPrice{}, err
	}
	price, err := decodeBudgetTokenPrice(slot, accounts, config, reference, budgetSOLReserve, debit)
	var hold *BudgetHold
	if errors.As(err, &hold) && hold.Reason == "stale_reserve_price" {
		var instructions []compiledInstruction
		for _, row := range []struct {
			address, mint string
			config        KaminoObservationConfig
		}{{budgetSOLReserve, budgetWrappedSOLMint, config}, {reference.DebtReserve, bridgeUSDC, reference}} {
			account := accountAt(accounts, row.address)
			if _, err = decodeKaminoReserve(account, row.mint, row.config); err != nil {
				return BudgetPrice{}, err
			}
			reserve, err := kamino.DecodeReserve(chainAccount(account, row.address))
			if err != nil {
				return BudgetPrice{}, err
			}
			instructions = append(instructions, kaminoCompiled(kamino.RefreshReserve(kamino.RefreshReserveAccounts{
				Reserve: kaminoKey(row.address), LendingMarket: reserve.LendingMarket, Pyth: reserve.PythPrice,
				SwitchboardPrice: reserve.SwitchboardPriceAggregator, SwitchboardTWAP: reserve.SwitchboardTWAPAggregator, Scope: reserve.ScopePriceFeed,
			})))
		}
		slot, accounts, err = simulateBudgetRefreshInstructions(ctx, rpc, instructions, addresses, slot)
		if err != nil {
			return BudgetPrice{}, err
		}
		price, err = decodeBudgetTokenPrice(slot, accounts, config, reference, budgetSOLReserve, debit)
		price.Source = "unsigned-reserve-refresh-simulation"
	}
	if err != nil {
		return BudgetPrice{}, err
	}
	if price.Decimals != 9 {
		return BudgetPrice{}, budgetHold("native_sol_decimal_mismatch")
	}
	// Wrapped SOL uses lamport-denominated units. This relabeling values the
	// native fee debit; no wrapping, swap, custody creation or transfer occurs.
	price.Mint = nativeSOLBudgetAsset
	price.TokenProgram = "11111111111111111111111111111111"
	return price, nil
}
