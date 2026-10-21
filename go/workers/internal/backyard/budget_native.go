package backyard

import (
	"context"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

const budgetSOLReserve = "d4A2prbA2whesmvHaL88BH6Ewn5N4bTSU2Ze8P6Bc4Q"

var budgetWrappedSOLMint = solana.WrappedSol.String()

func ObserveNativeSOLBudgetPrice(ctx context.Context, rpc *chain.Client, view *View, minimumSlot int64) (BudgetPrice, error) {
	if rpc == nil || minimumSlot <= 0 {
		return BudgetPrice{}, budgetHold("invalid_price_observation_request")
	}
	reference := usdcReference
	config := KaminoObservationConfig{Program: kamino.ProgramID.String(), Market: fleet.KaminoMainMarket}
	addresses := []string{budgetSOLReserve, reference.DebtReserve, budgetWrappedSOLMint, fleet.USDCMint, budgetClockAddress}
	_, accounts, err := view.read(ctx, addresses, minimumSlot)
	if err != nil {
		return BudgetPrice{}, err
	}
	var instructions []compiledInstruction
	{
		for _, row := range []struct {
			address, mint string
			config        KaminoObservationConfig
		}{{budgetSOLReserve, budgetWrappedSOLMint, config}, {reference.DebtReserve, fleet.USDCMint, reference}} {
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
	}
	slot, accounts, err := simulateBudgetRefreshInstructions(ctx, rpc, instructions, addresses, minimumSlot)
	if err != nil {
		return BudgetPrice{}, err
	}
	price, err := decodeBudgetTokenPrice(slot, accounts, config, reference, budgetSOLReserve, budgetWrappedSOLMint, classicTokenProgram)
	if err != nil {
		return BudgetPrice{}, err
	}
	if price.Decimals != 9 {
		return BudgetPrice{}, budgetHold("native_sol_decimal_mismatch")
	}
	price.Mint = nativeSOLBudgetAsset
	price.TokenProgram = "11111111111111111111111111111111"
	return price, nil
}
