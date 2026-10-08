// Package autodeposit implements the Autodeposit family: surplus-lot
// projection, sweep decisions, durable claim custody, signed two-leg
// (pull/top-up) execution and recovery. The typed rules here are the Go port of
// crates/balance-sweep-autodeposit-trigger/src/lib.rs and the durable
// confirmation protocol in scripts/durable-autodeposit-confirmation.ts.
package autodeposit

import (
	"errors"
	"fmt"
	"time"
)

// ScheduleDelay is the fixed one-hour wait between observing wallet surplus and
// becoming eligible to sweep it. It is not configurable per target.
const ScheduleDelay = time.Hour

// LotStatus mirrors loyal_yield.balance_sweep_surplus_lot_status.
type LotStatus string

const (
	LotOpen       LotStatus = "open"
	LotSelected   LotStatus = "selected"
	LotConsumed   LotStatus = "consumed"
	LotDepleted   LotStatus = "depleted"
	LotSuppressed LotStatus = "suppressed"
)

// SurplusLot is one schedulable tranche of wallet surplus. RemainingAmountRaw is
// the only authoritative balance; OriginalAmountRaw bounds later restoration.
type SurplusLot struct {
	ID                 int64
	SourceEventID      int64
	SourceSignature    *string
	OriginalAmountRaw  int64
	RemainingAmountRaw int64
	EligibleAfter      time.Time
	Status             LotStatus
	Confidence         string
	Reason             string
	CreatedAt          time.Time
}

// PositiveDelta is a confirmed wallet balance increase above what the wallet
// already held, from one observed event.
type PositiveDelta struct {
	SourceEventID   int64
	SourceSignature *string
	AmountRaw       int64
	ObservedAt      time.Time
	Confidence      string
	Reason          string
}

// LotErrorCode distinguishes rejected financial input from other failures.
type LotErrorCode string

const (
	LotErrNonPositiveDelta    LotErrorCode = "non_positive_delta"
	LotErrNonPositiveOutflow  LotErrorCode = "non_positive_outflow"
	LotErrInvalidLotRemaining LotErrorCode = "invalid_lot_remaining"
	LotErrAmountOverflow      LotErrorCode = "amount_overflow"
)

// LotError rejects an amount transition that would corrupt custody accounting.
type LotError struct {
	Code               LotErrorCode
	LotID              int64
	RemainingAmountRaw int64
}

func (e *LotError) Error() string {
	switch e.Code {
	case LotErrNonPositiveDelta:
		return "positive delta must be greater than zero"
	case LotErrNonPositiveOutflow:
		return "negative outflow amount must be greater than zero"
	case LotErrInvalidLotRemaining:
		return fmt.Sprintf("lot %d has invalid remaining amount %d", e.LotID, e.RemainingAmountRaw)
	case LotErrAmountOverflow:
		return "autodeposit amount arithmetic exceeded int64"
	default:
		return "autodeposit lot error"
	}
}

// ScheduledEligibleAfter applies the fixed one-hour delay to an observation.
func ScheduledEligibleAfter(observedAt time.Time) time.Time {
	return observedAt.Add(ScheduleDelay)
}

// InitialSurplusAmount schedules the first observation of a wallet: the balance
// above a configured floor. A target without a floor never schedules, and a
// balance at or below the floor schedules nothing.
func InitialSurplusAmount(amountRaw int64, walletBalanceFloorRaw *int64) (int64, bool) {
	if walletBalanceFloorRaw == nil {
		return 0, false
	}
	surplus, ok := subChecked(amountRaw, *walletBalanceFloorRaw)
	if !ok || surplus <= 0 {
		return 0, false
	}
	return surplus, true
}

// PositiveDeltaSurplusAmount schedules only the newly created surplus: the
// increase above the floor, not the whole deposit. A deposit entirely below the
// floor, or one that only refills a drawdown, schedules nothing.
func PositiveDeltaSurplusAmount(amountAfterRaw, deltaAmountRaw int64, walletBalanceFloorRaw *int64) (int64, bool) {
	if deltaAmountRaw <= 0 || walletBalanceFloorRaw == nil {
		return 0, false
	}
	amountBeforeRaw, ok := subChecked(amountAfterRaw, deltaAmountRaw)
	if !ok {
		return 0, false
	}
	floor := *walletBalanceFloorRaw
	previous := surplusAboveFloor(amountBeforeRaw, floor)
	current := surplusAboveFloor(amountAfterRaw, floor)
	schedulable, ok := subChecked(current, previous)
	if !ok {
		return 0, false
	}
	if schedulable <= 0 {
		return 0, false
	}
	return schedulable, true
}

func surplusAboveFloor(amountRaw, floorRaw int64) int64 {
	if amountRaw > floorRaw {
		if above, ok := subChecked(amountRaw, floorRaw); ok {
			return above
		}
		return 0
	}
	return 0
}

func subChecked(a, b int64) (int64, bool) {
	diff := a - b
	if (b > 0 && diff > a) || (b < 0 && diff < a) {
		return 0, false
	}
	return diff, true
}

func addChecked(a, b int64) (int64, bool) {
	sum := a + b
	if (b > 0 && sum < a) || (b < 0 && sum > a) {
		return 0, false
	}
	return sum, true
}

// PositiveDeltaToLot derives a stored lot from a plain balance increase.
func PositiveDeltaToLot(nextLotID, sourceEventID int64, sourceSignature *string, amountRaw int64, observedAt time.Time) (SurplusLot, error) {
	confidence := "derived"
	reason := "wallet balance increase scheduled for autodeposit after one hour"
	return LotFromPositiveDelta(nextLotID, PositiveDelta{
		SourceEventID:   sourceEventID,
		SourceSignature: sourceSignature,
		AmountRaw:       amountRaw,
		ObservedAt:      observedAt,
		Confidence:      confidence,
		Reason:          reason,
	})
}

// LotFromPositiveDelta stamps a positive delta into a fresh open lot eligible
// after the fixed one-hour delay.
func LotFromPositiveDelta(nextLotID int64, delta PositiveDelta) (SurplusLot, error) {
	if delta.AmountRaw <= 0 {
		return SurplusLot{}, &LotError{Code: LotErrNonPositiveDelta}
	}
	if delta.ObservedAt.IsZero() {
		return SurplusLot{}, errors.New("autodeposit positive delta has no observation time")
	}
	return SurplusLot{
		ID:                 nextLotID,
		SourceEventID:      delta.SourceEventID,
		SourceSignature:    delta.SourceSignature,
		OriginalAmountRaw:  delta.AmountRaw,
		RemainingAmountRaw: delta.AmountRaw,
		EligibleAfter:      ScheduledEligibleAfter(delta.ObservedAt),
		Status:             LotOpen,
		Confidence:         delta.Confidence,
		Reason:             delta.Reason,
		CreatedAt:          delta.ObservedAt,
	}, nil
}

// ApplyExternalOutflowNewestFirst consumes an externally observed wallet
// outflow from open lots, newest lot first. External spending must not free the
// oldest money the user deposited first. It returns the amount actually
// consumed; open lots may run out before the outflow does.
func ApplyExternalOutflowNewestFirst(lots []SurplusLot, amountRaw int64) (int64, error) {
	if amountRaw <= 0 {
		return 0, &LotError{Code: LotErrNonPositiveOutflow}
	}
	sortLotsForExternalSpend(lots)
	remaining := amountRaw
	for i := len(lots) - 1; i >= 0; i-- {
		if remaining == 0 {
			break
		}
		lot := &lots[i]
		if lot.Status != LotOpen || lot.RemainingAmountRaw == 0 {
			continue
		}
		if lot.RemainingAmountRaw < 0 {
			return 0, &LotError{Code: LotErrInvalidLotRemaining, LotID: lot.ID, RemainingAmountRaw: lot.RemainingAmountRaw}
		}
		consumed := min64(remaining, lot.RemainingAmountRaw)
		next, ok := subChecked(lot.RemainingAmountRaw, consumed)
		if !ok {
			return 0, &LotError{Code: LotErrAmountOverflow}
		}
		lot.RemainingAmountRaw = next
		remaining, _ = subChecked(remaining, consumed)
		if lot.RemainingAmountRaw == 0 {
			lot.Status = LotDepleted
		}
	}
	consumed, ok := subChecked(amountRaw, remaining)
	if !ok {
		return 0, &LotError{Code: LotErrAmountOverflow}
	}
	return consumed, nil
}

// sortLotsForExternalSpend orders by (created_at, id) so the caller can walk
// newest-first, matching the legacy Rust sort before its reverse iteration.
func sortLotsForExternalSpend(lots []SurplusLot) {
	sortLots(lots, func(a, b SurplusLot) bool {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
}

func sortLots(lots []SurplusLot, less func(a, b SurplusLot) bool) {
	// Insertion sort keeps the port dependency-free; lot batches are bounded by
	// the projection batch limit.
	for i := 1; i < len(lots); i++ {
		for j := i; j > 0 && less(lots[j], lots[j-1]); j-- {
			lots[j], lots[j-1] = lots[j-1], lots[j]
		}
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
