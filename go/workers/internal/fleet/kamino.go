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

// ReserveSlotOrderMismatch means the returned account was updated after the
// RPC response's context slot. It is not usable evidence; callers may re-read
// the complete catalog, but must never clamp either slot or reuse these bytes.
type ReserveSlotOrderMismatch struct {
	Reserve        string
	ContextSlot    int64
	LastUpdateSlot int64
}

func (e *ReserveSlotOrderMismatch) Error() string {
	return fmt.Sprintf("reserve %s economic slot order is invalid: contextSlot=%d lastUpdateSlot=%d", e.Reserve, e.ContextSlot, e.LastUpdateSlot)
}

func DecodeKaminoReserve(account *chain.Account, identity ReserveIdentity, contextSlot int64, slotDuration time.Duration) (ReserveState, error) {
	return decodeKaminoReserve(account, identity, contextSlot, slotDuration)
}

// DecodeKaminoSourceReserve names the source-only call site. Structurally valid
// stale economics are decoded for both roles; Plan excludes them as targets,
// while the route revalidator refreshes a stale source before withdraw.
func DecodeKaminoSourceReserve(account *chain.Account, identity ReserveIdentity, contextSlot int64, slotDuration time.Duration) (ReserveState, error) {
	return decodeKaminoReserve(account, identity, contextSlot, slotDuration)
}

func decodeKaminoReserve(account *chain.Account, identity ReserveIdentity, contextSlot int64, slotDuration time.Duration) (ReserveState, error) {
	reserve, err := kamino.DecodeReserve(account)
	if err != nil || account.Key.String() != identity.Address {
		return ReserveState{}, fmt.Errorf("reserve %s envelope or layout drifted", identity.Address)
	}
	if reserve.LendingMarket.String() != identity.Market || reserve.LiquidityMint.String() != identity.Mint {
		return ReserveState{}, fmt.Errorf("reserve %s identity drifted", identity.Address)
	}
	if contextSlot <= 0 || slotDuration <= 0 {
		return ReserveState{}, fmt.Errorf("confirmed context and slot duration are required")
	}
	if reserve.LastUpdateSlot == 0 || reserve.LastUpdateSlot > math.MaxInt64 {
		return ReserveState{}, fmt.Errorf("reserve %s has no bounded last update", identity.Address)
	}
	lastUpdateSlot := int64(reserve.LastUpdateSlot)
	lag := contextSlot - lastUpdateSlot
	if lag < 0 {
		return ReserveState{}, &ReserveSlotOrderMismatch{Reserve: identity.Address, ContextSlot: contextSlot, LastUpdateSlot: lastUpdateSlot}
	}
	remainingSlots := maximumEconomicSlotLag - lag
	if remainingSlots < 0 {
		remainingSlots = 0
	}
	economicLifetime := time.Duration(remainingSlots) * slotDuration
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

	if reserve.ProtocolTakeRatePct > 100 {
		return ReserveState{}, fmt.Errorf("reserve %s take rate is invalid", identity.Address)
	}
	rates, err := reserve.Rates()
	if err != nil {
		return ReserveState{}, fmt.Errorf("reserve %s rates: %w", identity.Address, err)
	}
	supplyAPY := rates.SupplyAPY
	if !finite(supplyAPY) || supplyAPY < 0 || supplyAPY >= 0.5 {
		return ReserveState{}, fmt.Errorf("reserve %s APY is outside the production bound", identity.Address)
	}
	hash := sha256.Sum256(account.Data)
	return ReserveState{
		ReserveIdentity: identity, Slot: contextSlot, LastUpdateSlot: lastUpdateSlot,
		LastUpdateStale:        reserve.LastUpdateStale,
		EconomicSlotLag:        lag,
		SupplyAPYBPS:           int64(math.Round(supplyAPY * 10_000)),
		TotalSupplyUSDMicros:   int64(math.Round(totalSupply)),
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
