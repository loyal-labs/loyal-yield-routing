package autodeposit

import (
	"time"
)

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

type LotErrorCode string

const (
	LotErrAmountOverflow LotErrorCode = "amount_overflow"
)

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

func ScheduledEligibleAfter(observedAt time.Time) time.Time {
	return observedAt.Add(ScheduleDelay)
}

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
