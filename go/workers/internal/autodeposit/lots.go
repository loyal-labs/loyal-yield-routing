// Package autodeposit implements the Autodeposit family: surplus-lot
// projection, sweep decisions, durable claim custody, signed two-leg
// (pull/top-up) execution and recovery. The typed rules here are the Go port of
// 91694cd9^:crates/balance-sweep-autodeposit-trigger/src/lib.rs and the durable
// confirmation protocol in scripts/durable-autodeposit-confirmation.ts.
package autodeposit

import (
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

// LotErrorCode distinguishes rejected financial input from other failures.
type LotErrorCode string

const (
	LotErrAmountOverflow LotErrorCode = "amount_overflow"
)

// LotError rejects an amount transition that would corrupt custody accounting.
type LotError struct {
	Code LotErrorCode
}

func (e *LotError) Error() string {
	switch e.Code {
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

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
