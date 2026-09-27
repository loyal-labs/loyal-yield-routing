package backyardrwa

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"os"
	"sync"
	"sync/atomic"
	"time"
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
func (c *RPCClient) refreshObservationLagSlots(ctx context.Context) {
	observationLagMu.Lock()
	defer observationLagMu.Unlock()
	now := time.Now()
	if now.Sub(observationLagChecked) < time.Minute {
		return
	}
	observationLagChecked = now
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ms, err := c.recentSlotMillis(ctx)
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

// recentSlotMillis averages the chain's last five 60 s performance samples.
func (c *RPCClient) recentSlotMillis(ctx context.Context) (float64, error) {
	var samples []struct {
		NumSlots         int64 `json:"numSlots"`
		SamplePeriodSecs int64 `json:"samplePeriodSecs"`
	}
	if err := c.call(ctx, "getRecentPerformanceSamples", []any{5}, &samples); err != nil {
		return 0, err
	}
	var slots, secs int64
	for _, s := range samples {
		if s.NumSlots > 0 && s.SamplePeriodSecs > 0 {
			slots, secs = slots+s.NumSlots, secs+s.SamplePeriodSecs
		}
	}
	if slots == 0 {
		return 0, fmt.Errorf("no usable performance samples")
	}
	return float64(secs) * 1000 / float64(slots), nil
}

// BudgetPrice is an observation, not an assumed stablecoin peg. The producer
// must bind the mint/program/decimals and reserve/oracle account hashes to its
// coherent chain read. Upper and lower prices share the oracle quote unit.
type BudgetCreditBounds struct {
	TokenLowerSF [16]byte `json:"tokenLowerSf"`
	USDCUpperSF  [16]byte `json:"usdcUpperSf"`
}

type BudgetPrice struct {
	Credit           *BudgetCreditBounds `json:"credit,omitempty"`
	Source           string              `json:"source"`
	Mint             string              `json:"mint"`
	TokenProgram     string              `json:"tokenProgram"`
	Decimals         uint8               `json:"decimals"`
	TokenUpperSF     [16]byte            `json:"tokenUpperSf"`
	USDCLowerSF      [16]byte            `json:"usdcLowerSf"`
	ObservedSlot     int64               `json:"observedSlot"`
	ValidThroughSlot int64               `json:"validThroughSlot"`
	EvidenceSHA256   string              `json:"evidenceSha256"`
}

func (p BudgetPrice) valueUpper(raw uint64, mint, program string, slot int64) (int64, error) {
	if p.Mint != mint || p.TokenProgram != program || p.Decimals > 18 || p.ObservedSlot <= 0 || slot < p.ObservedSlot || slot > p.ValidThroughSlot || p.ValidThroughSlot < p.ObservedSlot || p.ValidThroughSlot-p.ObservedSlot > budgetMaxObservationLagCeilingSlots || !sha256Pattern.MatchString(p.EvidenceSHA256) {
		return 0, budgetHold("missing_stale_or_mismatched_usdc_valuation")
	}
	value, err := valueBetweenTokenRaw(raw, p.Decimals, 6, p.TokenUpperSF, p.USDCLowerSF, true)
	if err != nil || value > math.MaxInt64 {
		return 0, budgetHold("invalid_usdc_valuation")
	}
	return int64(value), nil
}

// A guaranteed output is valued downwards, using the other side of the same
// independently observed price interval. An upper debit quote is not a lower
// bound on what a swap receives.
func (p BudgetPrice) valueLower(raw uint64, mint, program string, slot int64) (int64, error) {
	if _, err := p.valueUpper(0, mint, program, slot); err != nil {
		return 0, err
	}
	if p.Credit == nil {
		return 0, budgetHold("missing_credit_valuation_bounds")
	}
	if littleInt(p.Credit.TokenLowerSF[:]).Cmp(littleInt(p.TokenUpperSF[:])) > 0 || littleInt(p.Credit.USDCUpperSF[:]).Cmp(littleInt(p.USDCLowerSF[:])) < 0 {
		return 0, budgetHold("invalid_credit_valuation_interval")
	}
	value, err := valueBetweenTokenRaw(raw, p.Decimals, 6, p.Credit.TokenLowerSF, p.Credit.USDCUpperSF, false)
	if err != nil || value > math.MaxInt64 {
		return 0, budgetHold("invalid_credit_valuation")
	}
	return int64(value), nil
}

const nativeSOLBudgetAsset = "native:SOL"

type ValuedTransactionCost struct {
	ExecutionCost       *PilotExecutionCost   `json:"executionCost,omitempty"`
	Debit               ExecutableDebit       `json:"debit"`
	Fee                 MessageFeeObservation `json:"fee"`
	SetupLamports       uint64                `json:"setupLamports"`
	TokenPrice          *BudgetPrice          `json:"tokenPrice,omitempty"`
	NativePrice         BudgetPrice           `json:"nativePrice"`
	MessageSHA256       string                `json:"messageSha256"`
	ObservationSlot     int64                 `json:"observationSlot"`
	ValidThroughSlot    int64                 `json:"validThroughSlot"`
	PrincipalMicros     int64                 `json:"principalMicros"`
	NetworkFeeMicros    int64                 `json:"networkFeeMicros"`
	SetupLamportsMicros int64                 `json:"setupLamportsMicros"`
	TotalMicros         int64                 `json:"totalMicros"`
}

// ValueTransactionCost counts source principal once and fees once. Mint rent
// or account setup paid in lamports is distinct from network fees; callers
// must supply its independently bounded amount, including zero only when no
// account-creation/rent debit is possible. No protocol fee may be omitted:
// fees taken from the token source belong in the measured executable debit.
func ValueTransactionCost(message []byte, debit ExecutableDebit, fee MessageFeeObservation, setupLamports uint64, tokenPrice, solPrice BudgetPrice, slot int64) (ValuedTransactionCost, error) {
	result := ValuedTransactionCost{MessageSHA256: sha256Bytes(message), ObservationSlot: slot, Debit: debit, Fee: fee, SetupLamports: setupLamports, NativePrice: solPrice}
	if _, err := checkedUnsignedMessage(message); err != nil {
		return result, err
	}
	if fee.MessageSHA256 != result.MessageSHA256 || fee.Slot <= 0 || fee.Slot > math.MaxInt64-budgetMaxObservationLagCeilingSlots || fee.Slot > slot || slot-fee.Slot > observationLagSlots() || fee.Lamports == 0 {
		return result, budgetHold("fee_message_or_slot_mismatch")
	}
	var err error
	if debit.Raw > 0 {
		result.TokenPrice = &tokenPrice
		if debit.Source == "" {
			return result, budgetHold("economic_source_missing")
		}
		result.PrincipalMicros, err = tokenPrice.valueUpper(debit.Raw, debit.Mint, debit.TokenProgram, slot)
		if err != nil {
			return result, err
		}
	}
	if solPrice.Decimals != 9 {
		return result, budgetHold("native_sol_decimal_mismatch")
	}
	result.NetworkFeeMicros, err = solPrice.valueUpper(fee.Lamports, nativeSOLBudgetAsset, "11111111111111111111111111111111", slot)
	if err != nil {
		return result, err
	}
	if setupLamports > 0 {
		result.SetupLamportsMicros, err = solPrice.valueUpper(setupLamports, nativeSOLBudgetAsset, "11111111111111111111111111111111", slot)
		if err != nil {
			return result, err
		}
	}
	result.TotalMicros, err = budgetSum(result.PrincipalMicros, result.NetworkFeeMicros, result.SetupLamportsMicros)
	// Validity is bounded by the oldest input, not the final observation.
	result.ValidThroughSlot = min(fee.Slot+observationLagSlots(), solPrice.ValidThroughSlot)
	if debit.Raw > 0 {
		result.ValidThroughSlot = min(result.ValidThroughSlot, tokenPrice.ValidThroughSlot)
	}
	return result, err
}

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
