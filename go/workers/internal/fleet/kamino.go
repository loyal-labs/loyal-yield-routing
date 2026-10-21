package fleet

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

const (
	maximumEconomicSlotLag     = int64(1_500)
	minimumPublicationLifetime = 70 * time.Second
)

func DecodeKaminoReserve(account *chain.Account, identity ReserveIdentity, contextSlot int64, slotsPerSecond float64) (ReserveState, error) {
	return decodeKaminoReserve(account, identity, contextSlot, slotsPerSecond)
}

func decodeKaminoReserve(account *chain.Account, identity ReserveIdentity, contextSlot int64, slotsPerSecond float64) (ReserveState, error) {
	reserve, err := kamino.DecodeReserve(account)
	if err != nil || account.Key.String() != identity.Address {
		return ReserveState{}, fmt.Errorf("reserve %s envelope or layout drifted", identity.Address)
	}
	if reserve.LendingMarket.String() != identity.Market || reserve.LiquidityMint.String() != identity.Mint {
		return ReserveState{}, fmt.Errorf("reserve %s identity drifted", identity.Address)
	}
	if contextSlot <= 0 || !(slotsPerSecond > 0) || math.IsInf(slotsPerSecond, 0) {
		return ReserveState{}, fmt.Errorf("confirmed context and the measured slot rate are required")
	}
	if reserve.LastUpdateSlot == 0 || reserve.LastUpdateSlot > math.MaxInt64 {
		return ReserveState{}, fmt.Errorf("reserve %s has no bounded last update", identity.Address)
	}
	lastUpdateSlot := int64(reserve.LastUpdateSlot)
	slot := max(contextSlot, lastUpdateSlot)
	lag := slot - lastUpdateSlot
	remainingSlots := maximumEconomicSlotLag - lag
	if remainingSlots < 0 {
		remainingSlots = 0
	}
	economicLifetime := time.Duration(float64(remainingSlots) / slotsPerSecond * float64(time.Second))
	// KLend check_reserve_status_and_version allows Active (0) and Hidden
	// (2); Hidden is not Obsolete (1). Reject unknown enum values rather than
	// treating every nonzero status as inactive or admitting future statuses.
	if (reserve.Status != 0 && reserve.Status != 2) || reserve.EmergencyMode {
		return ReserveState{}, fmt.Errorf("reserve %s is not routable: status=%d emergency=%t", identity.Address, reserve.Status, reserve.EmergencyMode)
	}
	if reserve.MintDecimals != 6 {
		return ReserveState{}, fmt.Errorf("reserve %s is not a supported six-decimal stablecoin", identity.Address)
	}

	available := float64(reserve.AvailableAmount)
	borrowed := scaledFraction(reserve.BorrowedAmountSF)
	protocolFees := scaledFraction(reserve.AccumulatedProtocolFeesSF)
	referrerFees := scaledFraction(reserve.AccumulatedReferrerFeesSF)
	pendingFees := scaledFraction(reserve.PendingReferrerFeesSF)
	totalSupply := math.Max(0, available+borrowed-protocolFees-referrerFees-pendingFees)
	if !finite(totalSupply) || totalSupply <= 0 || totalSupply > float64(math.MaxInt64) {
		return ReserveState{}, fmt.Errorf("reserve %s supply is invalid", identity.Address)
	}
	utilization := borrowed / totalSupply
	if !finite(utilization) || utilization < 0 || utilization > 1.01 {
		return ReserveState{}, fmt.Errorf("reserve %s utilization is invalid", identity.Address)
	}

	rates, err := reserve.Rates(slotsPerSecond)
	if err != nil {
		return ReserveState{}, fmt.Errorf("reserve %s rates: %w", identity.Address, err)
	}
	supplyAPY := rates.SupplyAPY
	if !finite(supplyAPY) || supplyAPY < 0 || supplyAPY >= 0.5 {
		return ReserveState{}, fmt.Errorf("reserve %s APY is outside the production bound", identity.Address)
	}
	hash := sha256.Sum256(account.Data)
	var availableRaw *int64
	if reserve.AvailableAmount <= math.MaxInt64 {
		value := int64(reserve.AvailableAmount)
		availableRaw = &value
	}
	depositLimit := reserve.DepositLimit
	return ReserveState{
		AvailableLiquidityRaw: availableRaw, DepositLimitRaw: &depositLimit,
		ReserveIdentity: identity, Slot: slot, LastUpdateSlot: lastUpdateSlot,
		LastUpdateStale:        reserve.LastUpdateStale,
		EconomicSlotLag:        lag,
		SupplyAPYBPS:           int64(math.Round(supplyAPY * 10_000)),
		TotalSupplyRaw:         int64(math.Round(totalSupply)),
		EconomicLifetimeMillis: economicLifetime.Milliseconds(),
		DataHash:               hex.EncodeToString(hash[:]),
	}, nil
}

func scaledFraction(value [16]byte) float64 {
	low := binary.LittleEndian.Uint64(value[:8])
	high := binary.LittleEndian.Uint64(value[8:16])
	return float64(high)*16 + float64(low)/float64(uint64(1)<<60)
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
