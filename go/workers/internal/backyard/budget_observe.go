package backyard

import (
	"encoding/binary"
	"math"
	"strconv"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

const budgetClockAddress = "SysvarC1ock11111111111111111111111111111111"
const valuationMarginBPS uint16 = 100

func decodeBudgetTokenPrice(slot int64, accounts []ConfirmedAccount, config, reference KaminoObservationConfig, reserveAddress, tokenMint, tokenProgram string) (BudgetPrice, error) {
	if slot > math.MaxInt64-observationLagCeilingSlots {
		return BudgetPrice{}, staleHold("invalid_price_observation_slot")
	}
	clock := accountAt(accounts, budgetClockAddress)
	if clock.Owner != "Sysvar1111111111111111111111111111111111111" || clock.Executable || len(clock.Data) != 40 {
		return BudgetPrice{}, staleHold("invalid_valuation_clock")
	}
	now := int64(binary.LittleEndian.Uint64(clock.Data[32:40]))
	if now <= 0 {
		return BudgetPrice{}, staleHold("invalid_valuation_clock")
	}
	tokenAccount, referenceAccount := accountAt(accounts, reserveAddress), accountAt(accounts, reference.DebtReserve)
	token, err := decodeKaminoReserve(tokenAccount, tokenMint, config)
	if err != nil {
		return BudgetPrice{}, err
	}
	usdc, err := decodeKaminoReserve(referenceAccount, fleet.USDCMint, reference)
	if err != nil {
		return BudgetPrice{}, err
	}
	for _, account := range []ConfirmedAccount{tokenAccount, referenceAccount} {
		updated := int64(binary.LittleEndian.Uint64(account.Data[264:272]))
		if updated <= 0 || updated > now || now-updated > kaminoOracleMaxAgeSecs {
			return BudgetPrice{}, &PreSendHold{Reason: holdStale, Details: map[string]string{"cause": "reserve_price", "reserve": account.Address, "chainUnix": strconv.FormatInt(now, 10), "priceUpdatedUnix": strconv.FormatInt(updated, 10)}}
		}
	}
	mint := accountAt(accounts, tokenMint)
	usdcMint := accountAt(accounts, fleet.USDCMint)
	if err := validateExecutionMint(mint, tokenProgram, token.mintDecimals); err != nil {
		return BudgetPrice{}, err
	}
	if validateExecutionMint(usdcMint, classicTokenProgram, 6) != nil || usdc.mintDecimals != 6 {
		return BudgetPrice{}, staleHold("usdc_reference_metadata_mismatch")
	}
	upper, lower := token.terms.MarketPriceSF, usdc.terms.MarketPriceSF
	if tokenMint != fleet.USDCMint {
		upper, err = UpperPriceMargin(upper, valuationMarginBPS)
		if err != nil {
			return BudgetPrice{}, err
		}
		lower, err = lowerPriceMargin(lower, valuationMarginBPS)
		if err != nil {
			return BudgetPrice{}, err
		}
	}
	return BudgetPrice{Source: routeRefreshValuationSource, Mint: tokenMint, TokenProgram: mint.Owner, Decimals: token.mintDecimals, TokenUpperSF: upper, USDCLowerSF: lower, ObservedSlot: slot, ValidThroughSlot: slot + observationLagSlots(), EvidenceSHA256: hashConfirmedAccounts(accounts)}, nil
}
