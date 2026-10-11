package backyard

import (
	"errors"
	"testing"
)

// twentyBPSStepCost is a priced step cost of 20 bps of the borrowed debt
// (raw ~ USDC micros in these fixtures) plus three $0.01 transactions: the
// economics the leverage fixtures were written against.
func twentyBPSStepCost(raw uint64) (leverageStepCost, error) {
	return leverageStepLegs{EntryInMicros: int64(raw), EntryOutMicros: int64(raw) - int64(raw/500), ExitInMicros: 1, ExitOutMicros: 1, NetworkMicros: 30_000}.cost()
}

// measuredStepCost prices a step at measured AUTO quotes (2026-10-11, $100k):
// PYUSD->AUTO +0.003% (a gain, which is not a credit), AUTO->PYUSD -0.064%,
// a zero origination fee, and six transactions at about $0.002 each.
func measuredStepCost(feeBPS int64) leverageStepPricer {
	return func(raw uint64) (leverageStepCost, error) {
		in := int64(raw)
		return leverageStepLegs{BorrowFeeMicros: in * feeBPS / 10_000, EntryInMicros: in, EntryOutMicros: in + in*3/100_000,
			ExitInMicros: in + in*3/100_000, ExitOutMicros: in + in*3/100_000 - in*64/100_000, NetworkMicros: 12_000}.cost()
	}
}

func quoteOutage(uint64) (leverageStepCost, error) {
	return leverageStepCost{}, errors.New("jupiter quote unavailable")
}

// The expected cost floors each swap's loss at zero, charges both directions
// and the origination fee, and an unknown leg is unknown, never zero.
func TestLeverageStepCostIsWhatCapitalLoses(t *testing.T) {
	t.Parallel()
	c, err := measuredStepCost(0)(100_000_000_000)
	if err != nil || c.EntrySwapLossMicros != 0 || c.ExitSwapLossMicros != 64_000_000 || c.BorrowFeeMicros != 0 || c.TotalMicros != 64_012_000 {
		t.Fatalf("measured AUTO step: %+v %v", c, err)
	}
	for name, legs := range map[string]leverageStepLegs{
		"no exit quote":   {EntryInMicros: 1, EntryOutMicros: 1, ExitInMicros: 1, NetworkMicros: 1},
		"no network fee":  {EntryInMicros: 1, EntryOutMicros: 1, ExitInMicros: 1, ExitOutMicros: 1},
		"no entry quote":  {ExitInMicros: 1, ExitOutMicros: 1, NetworkMicros: 1},
		"negative borrow": {BorrowFeeMicros: -1, EntryInMicros: 1, EntryOutMicros: 1, ExitInMicros: 1, ExitOutMicros: 1, NetworkMicros: 1},
	} {
		if _, err := legs.cost(); err == nil {
			t.Fatalf("%s priced", name)
		}
	}
}
