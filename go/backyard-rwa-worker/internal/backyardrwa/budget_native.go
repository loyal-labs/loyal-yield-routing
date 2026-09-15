package backyardrwa

import (
	"context"
	"errors"
)

// Read-only price reference discovered at confirmed slot 444381476 by exact
// program, market and mint filters. This is NOT an executable runtime lane.
const budgetSOLReserve = "d4A2prbA2whesmvHaL88BH6Ewn5N4bTSU2Ze8P6Bc4Q"
const budgetSOLMarket = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"
const budgetWrappedSOLMint = "So11111111111111111111111111111111111111112"

func ObserveNativeSOLBudgetPrice(ctx context.Context, rpc *RPCClient, minimumSlot int64) (BudgetPrice, error) {
	if rpc == nil || minimumSlot <= 0 {
		return BudgetPrice{}, budgetHold("invalid_price_observation_request")
	}
	reference, err := pinnedKaminoObservationConfig()
	if err != nil {
		return BudgetPrice{}, err
	}
	config := KaminoObservationConfig{Program: kaminoProgram, Market: budgetSOLMarket}
	debit := ExecutableDebit{Mint: budgetWrappedSOLMint, TokenProgram: classicTokenProgram, Raw: 1}
	addresses := []string{budgetSOLReserve, reference.DebtReserve, budgetWrappedSOLMint, bridgeUSDC, budgetClockAddress}
	slot, accounts, err := rpc.GetMultipleAccounts(ctx, addresses, minimumSlot)
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
			// Preserve the four exact optional oracle account positions. A zero
			// key is the Anchor program-id sentinel, not an omitted account.
			metas := []accountMeta{kaminoMeta(row.address, false, true), kaminoMeta(row.config.Market, false, false)}
			// Reserve storage is Scope, Switchboard price/twap, Pyth;
			// refreshReserve ABI is Pyth, Switchboard price/twap, Scope.
			for _, offset := range []int{5224, 5160, 5192, 5112} {
				oracle := keyString(account.Data[offset : offset+32])
				if oracle == "" {
					oracle = kaminoProgram
				}
				metas = append(metas, kaminoMeta(oracle, false, false))
			}
			instructions = append(instructions, compiledInstruction{program: mustKey(kaminoProgram), accounts: metas, data: append([]byte(nil), kaminoRefreshReserve...)})
		}
		slot, accounts, err = rpc.simulateBudgetRefreshInstructions(ctx, instructions, addresses, slot)
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
