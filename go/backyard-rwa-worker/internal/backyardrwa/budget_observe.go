package backyardrwa

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"strconv"
)

const budgetClockAddress = "SysvarC1ock11111111111111111111111111111111"
const budgetPriceMaxAgeSeconds int64 = 60
const budgetPriceMarginBPS uint16 = 100

// ObserveBudgetTokenPrice reads the bound token reserve and the existing
// Prime USDC reference in one coherent batch with both mints and chain Clock.
// This is valuation-only: it does not broaden execution routes or authority.
// Native SOL fees need a separate reviewed price observation and cannot fall
// back to an assumed dollar price when this token observer does not cover them.
func ObserveBudgetTokenPrice(ctx context.Context, rpc *RPCClient, lane string, debit ExecutableDebit, minimumSlot int64) (BudgetPrice, error) {
	if rpc == nil || minimumSlot <= 0 || debit.Raw == 0 {
		return BudgetPrice{}, budgetHold("invalid_price_observation_request")
	}
	if lane == "" {
		lane = RouteID
	}
	route, err := runtimeRoute(lane)
	if err != nil {
		return BudgetPrice{}, err
	}
	reference, err := pinnedKaminoObservationConfig()
	if err != nil {
		return BudgetPrice{}, err
	}
	reserveAddress := reference.DebtReserve
	config := reference
	switch debit.Mint {
	case bridgeUSDC:
	case route.Kamino.CollateralMint:
		reserveAddress = route.Kamino.CollateralReserve
		config = route.Kamino
	case route.Kamino.DebtMint:
		reserveAddress = route.Kamino.DebtReserve
		config = route.Kamino
	default:
		return BudgetPrice{}, budgetHold("unbound_valuation_mint")
	}
	addresses := uniqueNonzero([]string{reserveAddress, reference.DebtReserve, debit.Mint, bridgeUSDC, budgetClockAddress})
	slot, accounts, err := rpc.GetMultipleAccounts(ctx, addresses, minimumSlot)
	if err != nil {
		return BudgetPrice{}, err
	}
	price, err := decodeBudgetTokenPrice(slot, accounts, config, reference, reserveAddress, debit)
	var hold *BudgetHold
	if !errors.As(err, &hold) || hold.Reason != "stale_reserve_price" {
		return price, err
	}
	// Refresh only in simulation, without loading a signer. This captured
	// state is neither a landed refresh nor signature-verified evidence.
	slot, accounts, err = rpc.simulateBudgetReserveRefresh(ctx, lane, addresses, slot)
	if err != nil {
		return BudgetPrice{}, err
	}
	price, err = decodeBudgetTokenPrice(slot, accounts, config, reference, reserveAddress, debit)
	price.Source = "unsigned-reserve-refresh-simulation"
	return price, err
}

func decodeBudgetTokenPrice(slot int64, accounts []ConfirmedAccount, config, reference KaminoObservationConfig, reserveAddress string, debit ExecutableDebit) (BudgetPrice, error) {
	if slot > math.MaxInt64-budgetMaxObservationLagSlots {
		return BudgetPrice{}, budgetHold("invalid_price_observation_slot")
	}
	clock := accountAt(accounts, budgetClockAddress)
	if clock.Owner != "Sysvar1111111111111111111111111111111111111" || clock.Executable || len(clock.Data) != 40 {
		return BudgetPrice{}, budgetHold("invalid_valuation_clock")
	}
	now := int64(binary.LittleEndian.Uint64(clock.Data[32:40]))
	if now <= 0 {
		return BudgetPrice{}, budgetHold("invalid_valuation_clock")
	}
	tokenAccount, referenceAccount := accountAt(accounts, reserveAddress), accountAt(accounts, reference.DebtReserve)
	token, err := decodeKaminoReserve(tokenAccount, debit.Mint, config)
	if err != nil {
		return BudgetPrice{}, err
	}
	usdc, err := decodeKaminoReserve(referenceAccount, bridgeUSDC, reference)
	if err != nil {
		return BudgetPrice{}, err
	}
	for _, account := range []ConfirmedAccount{tokenAccount, referenceAccount} {
		updated := int64(binary.LittleEndian.Uint64(account.Data[264:272]))
		if updated <= 0 || updated > now || now-updated > budgetPriceMaxAgeSeconds {
			return BudgetPrice{}, &BudgetHold{Reason: "stale_reserve_price", Details: map[string]string{"reserve": account.Address, "chainUnix": strconv.FormatInt(now, 10), "priceUpdatedUnix": strconv.FormatInt(updated, 10), "maximumAgeSeconds": strconv.FormatInt(budgetPriceMaxAgeSeconds, 10)}}
		}
	}
	mint := accountAt(accounts, debit.Mint)
	usdcMint := accountAt(accounts, bridgeUSDC)
	if mint.Owner != debit.TokenProgram || (mint.Owner != classicTokenProgram && mint.Owner != token2022Program) || mint.Executable || len(mint.Data) < 82 || mint.Data[45] != 1 || mint.Data[44] != token.mintDecimals {
		return BudgetPrice{}, budgetHold("valuation_mint_metadata_mismatch")
	}
	if err := validateExecutionMint(mint, debit.TokenProgram, token.mintDecimals); err != nil {
		return BudgetPrice{}, err
	}
	if usdcMint.Owner != classicTokenProgram || usdcMint.Executable || len(usdcMint.Data) != 82 || usdcMint.Data[45] != 1 || usdcMint.Data[44] != 6 || usdc.mintDecimals != 6 {
		return BudgetPrice{}, budgetHold("usdc_reference_metadata_mismatch")
	}
	upper, lower := token.marketPriceSF, usdc.marketPriceSF
	if debit.Mint != bridgeUSDC {
		upper, err = UpperPriceMargin(upper, budgetPriceMarginBPS)
		if err != nil {
			return BudgetPrice{}, err
		}
		lower, err = lowerPriceMargin(lower, budgetPriceMarginBPS)
		if err != nil {
			return BudgetPrice{}, err
		}
	}
	tokenLower, usdcUpper := token.marketPriceSF, usdc.marketPriceSF
	if debit.Mint != bridgeUSDC {
		tokenLower, err = lowerPriceMargin(tokenLower, budgetPriceMarginBPS)
		if err != nil {
			return BudgetPrice{}, err
		}
		usdcUpper, err = UpperPriceMargin(usdcUpper, budgetPriceMarginBPS)
		if err != nil {
			return BudgetPrice{}, err
		}
	}
	return BudgetPrice{Credit: &BudgetCreditBounds{tokenLower, usdcUpper}, Source: "confirmed-chain-accounts", Mint: debit.Mint, TokenProgram: mint.Owner, Decimals: token.mintDecimals, TokenUpperSF: upper, USDCLowerSF: lower, ObservedSlot: slot, ValidThroughSlot: slot + budgetMaxObservationLagSlots, EvidenceSHA256: hashConfirmedAccounts(accounts)}, nil
}
