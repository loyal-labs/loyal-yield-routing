package backyard

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"math/big"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// A destination is a flat lane a flat vault's entry would go to. Its batch is
// one refreshed capture of its reserves (observeSelectorDestinationBatch),
// the same kind of capture the tick values from, and these checks are the
// chain facts its legs' builders take as given.

// UserState/FarmState fields match farms-sdk 3.2.14 Borsh layouts, including
// discriminators. A token-loop quote cannot silently assume manual farm setup.
// An absent obligation has no registration yet: the lane's initializer
// creates the obligation and registers it with these farms in one
// transaction (compileKaminoInitializationMessage).
func validateSelectorFarms(v *Vault, route RuntimeRoute, accounts []ConfirmedAccount) error {
	obligationPresent := accountAt(accounts, route.Kamino.Obligation).Lamports != 0
	for _, pair := range [][2]string{{route.CollateralFarm, route.ObligationCollateralFarm}, {route.DebtFarm, route.ObligationDebtFarm}} {
		if pair[0] == "" && pair[1] == "" {
			continue
		}
		derived, err := deriveKaminoObligationFarmUserState(pair[0], route.Kamino.Obligation)
		if err != nil || derived != pair[1] {
			return budgetHoldFrom(err, "selector_farm_binding_invalid")
		}
		farm, user := accountAt(accounts, pair[0]), accountAt(accounts, pair[1])
		if farm.Owner != kamino.FarmsProgramID.String() || farm.Executable || farm.Lamports == 0 || len(farm.Data) != 8336 || !bytes.Equal(farm.Data[:8], []byte{198, 102, 216, 74, 63, 66, 163, 190}) ||
			!sameKey(farm.Data[7328:7360], route.Kamino.MarketAuthority) || farm.Data[7361] != 0 || farm.Data[7362] != 1 {
			return budgetHold("selector_farm_unavailable")
		}
		if !obligationPresent {
			continue
		}
		if state, err := kamino.DecodeFarmUserState(chainAccount(user, user.Address)); err != nil || !sameKey(state.FarmState[:], pair[0]) ||
			!sameKey(state.Owner[:], v.squads) || !state.IsFarmDelegated || !sameKey(state.Delegatee[:], route.Kamino.Obligation) {
			return budgetHold("selector_farm_registration_unavailable")
		}
	}
	return nil
}

// selectorDestinationAccounts is a flat destination's refreshed batch, its
// position, and every check its entry's legs depend on.
func selectorDestinationAccounts(ctx context.Context, v *Vault, rpc *chain.Client, view *View, route RuntimeRoute, minimumSlot int64) (int64, []ConfirmedAccount, KaminoPosition, error) {
	var empty KaminoPosition
	slot, accounts, position, err := observeSelectorDestinationBatch(ctx, v, rpc, route, minimumSlot)
	if err != nil {
		return 0, nil, empty, err
	}
	if position.HasPosition || position.CollateralDepositedRaw != 0 || position.DebtRaw != 0 {
		return 0, nil, empty, budgetHold("selector_destination_not_flat")
	}
	custody, err := validateSelectorDestinationCommon(v, route, slot, accounts, position)
	if err != nil {
		return 0, nil, empty, err
	}
	if custody != 0 {
		return 0, nil, empty, budgetHold("selector_destination_not_flat")
	}
	return slot, accounts, position, nil
}

// observeSelectorDestinationBatch is the destination lane's batch and
// position, valued like the tick's: one bank captured after the lane's
// reserve refreshes (an unsigned simulation, never a broadcast), tolerating
// an absent optional obligation or farm. bridgeDelegate's captured lamports
// differ by the simulated fee; every freshness, ownership, capacity and
// funding check still runs fail-closed on it.
func observeSelectorDestinationBatch(ctx context.Context, v *Vault, rpc *chain.Client, route RuntimeRoute, minimumSlot int64) (int64, []ConfirmedAccount, KaminoPosition, error) {
	var empty KaminoPosition
	addresses := []string{route.Kamino.Market, route.Kamino.Obligation, route.Kamino.CollateralReserve, route.Kamino.DebtReserve,
		route.Kamino.CollateralMint, route.Kamino.DebtMint, route.CollateralCustody, route.DebtCustody, route.CollateralLiquiditySupply,
		route.DebtLiquiditySupply, route.DebtFeeReceiver, route.CollateralReceiptMint, route.CollateralReceiptSupply,
		route.CollateralFarm, route.ObligationCollateralFarm, route.DebtFarm, route.ObligationDebtFarm,
		budgetClockAddress, v.squads, bridgeDelegate}
	if !v.trustful() {
		// The FSj27 config and report ticket the destination checks
		// (validateSelectorDestinationCommon).
		addresses = append(addresses, v.Strategy, reportTicketPDA)
	}
	addresses = append(addresses, usdcReference.DebtReserve)
	optional := uniqueNonzero([]string{route.Kamino.Obligation, route.ObligationCollateralFarm, route.ObligationDebtFarm})
	slot, accounts, err := simulateLanesReserveRefreshOptional(ctx, v, rpc, []string{route.Lane}, uniqueNonzero(addresses), optional, minimumSlot)
	if err != nil {
		return 0, nil, empty, err
	}
	position, err := observeKaminoFromFixedAccounts(slot, accounts, route.Kamino)
	if err != nil {
		return 0, nil, empty, err
	}
	return slot, accounts, position, nil
}

// validateSelectorDestinationCommon runs every destination check that does not
// depend on flatness, and returns the observed collateral custody balance so
// each caller applies its own flatness contract.
func validateSelectorDestinationCommon(v *Vault, route RuntimeRoute, slot int64, accounts []ConfirmedAccount, position KaminoPosition) (uint64, error) {
	if !v.trustful() {
		if _, err := decodeObservedAdaptorConfig(v, accountAt(accounts, v.Strategy)); err != nil {
			return 0, err
		}
		ticket, err := decodeObservedReportTicket(v, accountAt(accounts, reportTicketPDA))
		if err != nil || ticket.Armed || ticket.LastConsumedSequence >= uint64(slot) {
			return 0, budgetHoldFrom(err, "selector_destination_ticket_unavailable")
		}
	}
	var err error
	if err = validateSelectorFarms(v, route, accounts); err != nil {
		return 0, err
	}
	for _, side := range []struct {
		reserve, mint, program, supply, farm string
		decimals                             uint8
	}{
		{route.Kamino.CollateralReserve, route.Kamino.CollateralMint, route.CollateralTokenProgram, route.CollateralLiquiditySupply, route.CollateralFarm, position.CollateralDecimals},
		{route.Kamino.DebtReserve, route.Kamino.DebtMint, route.DebtTokenProgram, route.DebtLiquiditySupply, route.DebtFarm, position.DebtDecimals},
	} {
		a := accountAt(accounts, side.reserve)
		farmOffset := 64
		if side.reserve == route.Kamino.DebtReserve {
			farmOffset = 96
		}
		farmKey := side.farm
		if farmKey == "" {
			farmKey = "11111111111111111111111111111111"
		}
		if !sameKey(a.Data[160:192], side.supply) || !sameKey(a.Data[408:440], side.program) || !sameKey(a.Data[farmOffset:farmOffset+32], farmKey) {
			return 0, budgetHold("selector_destination_reserve_binding_changed")
		}
		if err = validateExecutionMint(accountAt(accounts, side.mint), side.program, side.decimals); err != nil {
			return 0, err
		}
	}
	c := accountAt(accounts, route.Kamino.CollateralReserve)
	d := accountAt(accounts, route.Kamino.DebtReserve)
	if !sameKey(c.Data[2560:2592], route.CollateralReceiptMint) || !sameKey(c.Data[2600:2632], route.CollateralReceiptSupply) || !sameKey(d.Data[192:224], route.DebtFeeReceiver) {
		return 0, budgetHold("selector_destination_reserve_binding_changed")
	}
	receipt := accountAt(accounts, route.CollateralReceiptMint)
	if receipt.Owner != classicTokenProgram || receipt.Executable || receipt.Lamports == 0 || len(receipt.Data) != 82 || receipt.Data[45] != 1 || binary.LittleEndian.Uint32(receipt.Data[:4]) != 1 || !sameKey(receipt.Data[4:36], route.Kamino.MarketAuthority) {
		return 0, budgetHold("selector_destination_receipt_mint_unavailable")
	}
	collateralCustody := uint64(0)
	for _, b := range []struct{ address, mint, authority, program string }{
		{route.CollateralCustody, route.Kamino.CollateralMint, v.squads, route.CollateralTokenProgram},
		{route.DebtCustody, route.Kamino.DebtMint, v.squads, route.DebtTokenProgram},
		{route.CollateralLiquiditySupply, route.Kamino.CollateralMint, route.Kamino.MarketAuthority, route.CollateralTokenProgram},
		{route.DebtLiquiditySupply, route.Kamino.DebtMint, route.Kamino.MarketAuthority, route.DebtTokenProgram},
		{route.DebtFeeReceiver, route.Kamino.DebtMint, route.Kamino.MarketAuthority, route.DebtTokenProgram},
		{route.CollateralReceiptSupply, route.CollateralReceiptMint, route.Kamino.MarketAuthority, route.CollateralTokenProgram},
	} {
		a := accountAt(accounts, b.address)
		custody, err := DecodeTokenCustody(a.Owner, a.Data, mustKey(b.mint), mustKey(b.authority))
		if err != nil || a.Owner != b.program || a.Executable || a.Lamports == 0 {
			return 0, budgetHoldFrom(err, "selector_destination_custody_unavailable")
		}
		if b.address == route.CollateralCustody {
			collateralCustody = custody.Raw
		}
	}
	return collateralCustody, nil
}

// unleveredEntryCapacityDebtRaw bounds a B2 1x destination entry by the
// collateral reserve's remaining deposit limit, valued in debt-mint units
// (floored, less 1%) so the caller's existing debt-price conversion applies
// unchanged. No borrow capacity is involved.
func unleveredEntryCapacityDebtRaw(position KaminoPosition, accounts []ConfirmedAccount, route RuntimeRoute) (uint64, error) {
	collateral, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil || collateral.totalLiquiditySF == nil || collateral.depositLimitRaw == 0 {
		return 0, err
	}
	deposited, err := ceilScaledBigFraction(collateral.totalLiquiditySF)
	if err != nil || deposited >= collateral.depositLimitRaw {
		return 0, err
	}
	room, err := valueBetweenTokenRaw(collateral.depositLimitRaw-deposited, position.CollateralDecimals, position.DebtDecimals, position.CollateralPriceSF, position.DebtPriceSF, false)
	if err != nil {
		return 0, err
	}
	return min(room/100*99, math.MaxInt64), nil
}

// payoffReceiptsForLiquidity ceils the receipt count whose redemption at ANY
// future rate guarantees the liquidity target: the collateral rate only rises,
// so floor(wire * rateFuture) >= floor(wire * rateNow) >= the target.
func payoffReceiptsForLiquidity(totalLiquiditySF *big.Int, mintSupply, liquidity uint64) (uint64, error) {
	if liquidity == 0 || totalLiquiditySF == nil || totalLiquiditySF.Sign() <= 0 || mintSupply == 0 {
		return 0, budgetHold("selector_destination_receipt_bound_unavailable")
	}
	numerator := new(big.Int).Mul(new(big.Int).SetUint64(liquidity), new(big.Int).Lsh(new(big.Int).SetUint64(mintSupply), 60))
	receipts := new(big.Int).Add(numerator, totalLiquiditySF)
	receipts.Sub(receipts, big.NewInt(1))
	receipts.Quo(receipts, totalLiquiditySF)
	if !receipts.IsUint64() {
		return 0, budgetHold("selector_destination_receipt_bound_unavailable")
	}
	return receipts.Uint64(), nil
}
