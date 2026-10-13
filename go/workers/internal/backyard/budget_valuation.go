package backyard

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

const budgetMaxObservationLagSlots int64 = 32

// Our observation freshness window is ~13 s of chain time, not a fixed 32
// slots: at ~270 ms slots 32 slots is 8.6 s while observe-to-send takes 9-12 s,
// so most 09-26 moves failed the final re-check (13 tries, 10 restarts).
// observationLagSlots is ceil(13 s / measured slot time) clamped to
// [budgetMaxObservationLagSlots, budgetMaxObservationLagCeilingSlots]; 32 when
// unmeasured. Window producers and fresh age checks use it; window-width
// sanity checks on persisted evidence use the fixed ceiling so a later
// measurement never strands a record. Kamino/adaptor protocol limits stay 32.
// A lower later measurement only shortens a live window: a quote or recipe
// wider than the current window is refused and re-priced, never stretched.
const (
	budgetMaxObservationLagCeilingSlots int64 = 64
	observationWindowMillis                   = 13_000
)

var (
	observationLag         atomic.Int64
	observationLagMu       sync.Mutex
	observationLagChecked  time.Time
	observationLagMeasured time.Time
)

func observationLagSlots() int64 {
	if v := observationLag.Load(); v > 0 {
		return v
	}
	return budgetMaxObservationLagSlots
}

func observationLagForSlotMillis(ms float64) int64 {
	if !(ms > 0) {
		return budgetMaxObservationLagSlots
	}
	return min(max(int64(math.Ceil(observationWindowMillis/ms)), budgetMaxObservationLagSlots), budgetMaxObservationLagCeilingSlots)
}

// refreshObservationLagSlots re-measures the slot time at most once a minute,
// at the start of a tick so one tick sees one window. A failed read keeps the
// last value for 10 minutes, then falls back to 32 slots.
func refreshObservationLagSlots(ctx context.Context, c *chain.Client) {
	observationLagMu.Lock()
	defer observationLagMu.Unlock()
	now := time.Now()
	if now.Sub(observationLagChecked) < time.Minute {
		return
	}
	observationLagChecked = now
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ms, err := recentSlotMillis(ctx, c)
	if err != nil {
		if now.Sub(observationLagMeasured) > 10*time.Minute {
			observationLag.Store(0)
		}
		return
	}
	observationLagMeasured = now
	if slots := observationLagForSlotMillis(ms); observationLag.Swap(slots) != slots {
		_, _ = fmt.Fprintf(os.Stderr, "backyard-rwa-worker: observation window slots=%d slot_ms=%.0f\n", slots, ms)
	}
}

// recentSlotMillis is the chain's measured slot time.
func recentSlotMillis(ctx context.Context, c *chain.Client) (float64, error) {
	slotsPerSecond, err := c.SlotsPerSecond(ctx)
	if err != nil {
		return 0, err
	}
	return 1000 / slotsPerSecond, nil
}

// BudgetPrice is an observation, not an assumed stablecoin peg. The producer
// must bind the mint/program/decimals and reserve/oracle account hashes to its
// coherent chain read. Upper and lower prices share the oracle quote unit.
type BudgetPrice struct {
	Source           string   `json:"source"`
	Mint             string   `json:"mint"`
	TokenProgram     string   `json:"tokenProgram"`
	Decimals         uint8    `json:"decimals"`
	TokenUpperSF     [16]byte `json:"tokenUpperSf"`
	USDCLowerSF      [16]byte `json:"usdcLowerSf"`
	ObservedSlot     int64    `json:"observedSlot"`
	ValidThroughSlot int64    `json:"validThroughSlot"`
	EvidenceSHA256   string   `json:"evidenceSha256"`
}

func (p BudgetPrice) valueUpper(raw uint64, mint, program string, slot int64) (int64, error) {
	if p.Mint != mint || p.TokenProgram != program || p.Decimals > 18 || p.ObservedSlot <= 0 || slot < p.ObservedSlot || slot > p.ValidThroughSlot || p.ValidThroughSlot < p.ObservedSlot || p.ValidThroughSlot-p.ObservedSlot > budgetMaxObservationLagCeilingSlots || !sha256Pattern.MatchString(p.EvidenceSHA256) {
		return 0, budgetHold("missing_stale_or_mismatched_usdc_valuation")
	}
	value, err := valueBetweenTokenRaw(raw, p.Decimals, 6, p.TokenUpperSF, p.USDCLowerSF, true)
	if err != nil || value > math.MaxInt64 {
		return 0, budgetHoldFrom(err, "invalid_usdc_valuation")
	}
	return int64(value), nil
}

const nativeSOLBudgetAsset = "native:SOL"

// UpperPriceMargin applies a caller-reviewed conservative basis-point margin
// to a positive observed price without float precision loss or u128 wrapping.
func UpperPriceMargin(price [16]byte, marginBPS uint16) ([16]byte, error) {
	var out [16]byte
	value := littleInt(price[:])
	if value.Sign() <= 0 {
		return out, budgetHold("invalid_usdc_valuation")
	}
	value.Mul(value, new(big.Int).SetUint64(10_000+uint64(marginBPS)))
	value.Add(value, big.NewInt(9_999))
	value.Div(value, big.NewInt(10_000))
	if value.BitLen() > 128 {
		return out, budgetHold("invalid_usdc_valuation")
	}
	bytes := value.Bytes()
	for i := range bytes {
		out[i] = bytes[len(bytes)-1-i]
	}
	return out, nil
}

func lowerPriceMargin(price [16]byte, marginBPS uint16) ([16]byte, error) {
	var out [16]byte
	if marginBPS >= 10_000 {
		return out, budgetHold("invalid_usdc_valuation")
	}
	value := littleInt(price[:])
	value.Mul(value, new(big.Int).SetUint64(10_000-uint64(marginBPS)))
	value.Div(value, big.NewInt(10_000))
	if value.Sign() <= 0 {
		return out, budgetHold("invalid_usdc_valuation")
	}
	bytes := value.Bytes()
	for i := range bytes {
		out[i] = bytes[len(bytes)-1-i]
	}
	return out, nil
}
