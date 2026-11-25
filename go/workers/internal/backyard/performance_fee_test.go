package backyard

import (
	"encoding/binary"
	"testing"
	"time"
)

var feeMonday = time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC).Unix()

func feeBank(f performanceFeeState, at, supply, public, cash int64) Snapshot {
	return Snapshot{feeCapitalSlot: 500 + f.Version, Trustful: true, MonitorsArmed: true, Slot: 500 + f.Version, ChainUnix: at,
		LPSupplyInclFeesRaw: supply, VoltrTotalValueRaw: public, PriorReportedNAVRaw: f.PublishedStrategy, SquadsIdleRaw: cash}
}

func feeStep(t *testing.T, f performanceFeeState, s Snapshot, gross, physicalStrategy, postCash, paid int64, hourly bool) performanceFeeEntry {
	t.Helper()
	e, err := f.assess(s, gross, physicalStrategy, postCash, paid, hourly)
	if err != nil || !e.valid() {
		t.Fatalf("assessment at %d: %+v / %v", s.ChainUnix, e, err)
	}
	return e
}

func feeAnchor(t *testing.T, at, cap int64) performanceFeeState {
	t.Helper()
	f := performanceFeeState{ActivationSlot: 400, WeeklyCapRaw: cap}
	s := feeBank(f, at, 1024, 1024, 0)
	return feeStep(t, f, s, 1024, 1024, 0, 0, true).After
}

func TestLoopingFeeCutoverHourlyAndRecovery(t *testing.T) {
	f := performanceFeeState{ActivationSlot: 400, WeeklyCapRaw: 1000}
	at := feeMonday - 86400
	s := feeBank(f, at, 1024, 1000, 0)
	s.FeeAccumulatorRaw = 32 // unharvested LP is already in effective supply.
	binary.LittleEndian.PutUint64(s.VoltrHighWaterMarkBits[:], feePriceBits(1280, 1024).Uint64())
	anchor := feeStep(t, f, s, 1220, 1220, 0, 0, true)
	if anchor.Assessed != 0 || anchor.After.PeakBits != feePriceBits(1280, 1024).String() {
		t.Fatal(anchor)
	}
	f = anchor.After
	for _, tc := range []struct {
		elapsed, gross, expected int64
		hourly                   bool
	}{
		{3600, 1200, 0, true}, {7200, 1280, 0, true}, {7500, 1480, 0, false},
		{10799, 1480, 0, true}, {10800, 1480, 20, true}, {10800, 1480, 0, true},
		{14400, 1300, 0, true}, {18000, 1480, 0, true}, {21600, 1580, 10, true},
	} {
		s = feeBank(f, at+tc.elapsed, 1024, f.PublishedValue, 0)
		e := feeStep(t, f, s, tc.gross, tc.gross, 0, 0, tc.hourly)
		if e.Assessed != tc.expected || e.After.PublishedValue != tc.gross-e.After.Outstanding {
			t.Fatalf("elapsed %d: %+v", tc.elapsed, e)
		}
		f = e.After
	}
	// A pre-cutover but unpublished gain above the historic Voltr mark is also excluded.
	fresh := feeAnchor(t, at, 1000)
	if fresh.PeakBits != feePriceBits(1024, 1024).String() {
		t.Fatal(fresh)
	}
	for _, mutate := range []func(*Snapshot){func(s *Snapshot) { s.AdminPerformanceFeeBPS = 1500 }, func(s *Snapshot) { s.Trustful = false }, func(s *Snapshot) { s.PriorReportedNAVRaw++ }, func(s *Snapshot) { s.Slot = 399 }} {
		s = feeBank(f, at+25000, 1024, f.PublishedValue, 0)
		mutate(&s)
		if _, err := f.assess(s, 1600, 1600, 0, 0, true); err == nil {
			t.Fatal("accepted changed fee terms, route, cutover or receipt")
		}
	}
}

func TestLoopingFeeCapitalFlowsAndCashGifts(t *testing.T) {
	at := feeMonday - 86400
	f := feeAnchor(t, at, 1000)
	// An extra NAV publishes a gain, but doesn't assess. A subsequent deposit
	// doubles effective supply at that price. Only the original shares earned it.
	f = feeStep(t, f, feeBank(f, at+600, 1024, 1024, 0), 1224, 1224, 0, 0, false).After
	s := feeBank(f, at+3600, 2048, 2448, 0)
	e := feeStep(t, f, s, 2448, 1224, 0, 0, true)
	if e.Assessed != 20 {
		t.Fatal("charged subscription principal", e)
	}
	f = e.After
	// Half the shares withdraw at the net price, and legacy fee LP is harvested.
	s = feeBank(f, at+7200, 1024, 1214, 0)
	s.FeeAccumulatorRaw = 0
	e = feeStep(t, f, s, 1234, 1234, 0, 0, true)
	if e.Assessed != 0 || e.After.Outstanding != 20 {
		t.Fatal("charged withdrawal or harvested LP", e)
	}
	// A gift into native USDC custody can arrive in the same interval as a
	// journaled sale. Only the journaled conversion explains strategy cash.
	f = feeAnchor(t, at, 1000)
	s = feeBank(f, at+3600, 1024, 1024, 700)
	s.performanceCashDelta = 100
	e = feeStep(t, f, s, 1624, 1624, 700, 0, true)
	if e.Assessed != 0 || e.OutsideCash != 600 {
		t.Fatal("charged outside cash", e)
	}
	f = e.After
	e = feeStep(t, f, feeBank(f, at+7200, 1024, 1624, 700), 1724, 1724, 700, 0, true)
	if e.Assessed != 10 || e.OutsideCash != 0 {
		t.Fatal("gift was charged again or later earned income skipped", e)
	}
	// Loss carryforward survives a gift while the position is underwater.
	f = feeAnchor(t, at, 1000)
	f = feeStep(t, f, feeBank(f, at+3600, 1024, 1024, 0), 824, 824, 0, 0, true).After
	f = feeStep(t, f, feeBank(f, at+7200, 1024, 824, 100), 924, 924, 100, 0, true).After
	e = feeStep(t, f, feeBank(f, at+10800, 1024, 924, 100), 1124, 1124, 100, 0, true)
	if e.Assessed != 0 {
		t.Fatal("charged recovery after a gift", e)
	}
}

func TestLoopingFeeMondayLimitAndPartialSettlement(t *testing.T) {
	const cap = 15
	f := feeAnchor(t, feeMonday-7200, cap)
	f = feeStep(t, f, feeBank(f, feeMonday-3600, 1024, 1024, 0), 1224, 1224, 0, 0, true).After
	if f.Outstanding != 20 || f.paymentRoom(feeMonday-1) != 0 || f.paymentRoom(feeMonday) != 15 {
		t.Fatal(f)
	}
	f = feeStep(t, f, feeBank(f, feeMonday, 1024, f.PublishedValue, 0), 1324, 1324, 0, 0, true).After
	if f.Payable != 30 {
		t.Fatal("gain through cutoff not included", f)
	}
	e := feeStep(t, f, feeBank(f, feeMonday+60, 1024, f.PublishedValue, 0), 1324, 1318, 0, 6, false)
	if e.After.PublishedValue != f.PublishedValue || e.After.Outstanding != 24 || e.After.paymentRoom(feeMonday+61) != 9 {
		t.Fatal("settling reserved USDC changed share price", e)
	}
	f = e.After
	s := feeBank(f, feeMonday+7*86400-1, 1024, f.PublishedValue, 0)
	if _, err := f.assess(s, 1318, 1308, 0, 10, false); err == nil {
		t.Fatal("Squads rolling reset bypassed the calendar limit")
	}
	f = feeStep(t, f, s, 1318, 1309, 0, 9, false).After
	if f.paymentRoom(s.ChainUnix) != 0 || f.paymentRoom(feeMonday+7*86400) != 15 {
		t.Fatal(f)
	}
	s = feeBank(f, feeMonday+7*86400+3, 1024, f.PublishedValue, 0)
	e = feeStep(t, f, s, 1509, 1494, 0, 15, true)
	if e.Assessed != 20 || e.After.Outstanding != 20 || e.After.Payable != 0 || e.After.PaidThisWeek != 15 {
		t.Fatal("post-cutoff assessment paid early or prior debt lost", e)
	}
}

func TestLoopingFeePaymentFundsCustodyBeforePayout(t *testing.T) {
	f := feeAnchor(t, feeMonday, 100_000_000)
	f.Outstanding, f.Payable = 30_000_000, 30_000_000
	for _, tc := range []struct {
		cash, idle, withdraw int64
		action               Action
		amount               int64
	}{
		{0, 100_000_000, 0, VoltrAllocateToSquads, 30_000_000},
		{8_000_000, 92_000_000, 0, VoltrAllocateToSquads, 22_000_000},
		{30_000_000, 70_000_000, 0, PayPerformanceFee, 30_000_000},
		{100_000_000, 0, 1, "", 0},
	} {
		s := base()
		s.Trustful = true
		s.ChainUnix = feeMonday + 60
		s.performanceFee = &f
		s.SquadsIdleRaw, s.VoltrIdleRaw, s.WithdrawalDemandRaw = tc.cash, tc.idle, tc.withdraw
		s.TotalVaultNAVRaw = 100_000_000
		s.StrategyNAVRaw = tc.cash
		s.PriorReportedNAVRaw = tc.cash
		s.VoltrTotalValueRaw = tc.cash + tc.idle
		s.feeAllowance = &StageAllowance{RemainingRaw: 100_000_000, MaxRaw: 100_000_000, ResetAt: s.ChainUnix + 100}
		bindSnapshotNAV(&s)
		got := Decide(s)
		if tc.action == "" {
			if got.Action == PayPerformanceFee || got.Reason == "fund_weekly_performance_fee" {
				t.Fatal("paid ahead of withdrawals", got)
			}
			continue
		}
		if got.Action != tc.action || got.AmountRaw != tc.amount || got.Validate() != nil {
			t.Fatalf("cash %d: %+v", tc.cash, got)
		}
	}
}

func TestLoopingFeeCarriesSubMicroAssessments(t *testing.T) {
	f := feeAnchor(t, feeMonday, 10)
	for i := int64(1); i <= 10; i++ {
		gross := 1024 + i
		e := feeStep(t, f, feeBank(f, feeMonday+3600*i, 1024, f.PublishedValue, 0), gross, gross, 0, 0, true)
		if (i == 10) != (e.Assessed == 1) {
			t.Fatal("fractional fees lost or collected early", i, e)
		}
		f = e.After
	}
	if f.Outstanding != 1 || f.Remainder != 0 {
		t.Fatal(f)
	}
}
