package earnmax

import (
	"testing"
	"time"
)

func earningsTime(day int) time.Time { return time.Date(2026, 10, day, 12, 0, 0, 0, time.UTC) }
func cost(v USDCRaw) *USDCRaw        { return &v }
func flowAt(day int, slot uint64, delta LPRaw, before, after uint64) Flow {
	f := Flow{At: earningsTime(day), Slot: slot, LPDelta: delta, PoolLPBefore: before, PoolLPAfter: after}
	f.Signature[0] = byte(slot)
	return f
}
func assertEarnings(t *testing.T, got *Earnings, performance, costChange, net float64) {
	t.Helper()
	if got == nil || got.PerformanceUsd != performance || got.SellingCostChangeUsd != costChange || got.NetEarnedUsd != net {
		t.Fatalf("earnings=%+v, want performance=%v costs=%v net=%v", got, performance, costChange, net)
	}
	for _, d := range got.Days {
		if diff := d.PerformanceUsd + d.SellingCostChangeUsd - d.NetEarnedUsd; diff > 0.000001 || diff < -0.000001 {
			t.Fatalf("day does not reconcile: %+v", d)
		}
	}
}
func TestEarningsSeparatesEstimateChangesAndKeepsRealLoss(t *testing.T) {
	h := History{Entries: []Entry{Deposit{Flow: flowAt(1, 1, 10, 90, 100), Amount: 100_000_000}}}
	days := []DayEnd{{At: earningsTime(1).Add(10 * time.Hour), Price: 10_000_000, PoolLP: 100}, {At: earningsTime(2).Add(10 * time.Hour), Price: 9_800_000, PoolLP: 100}}
	p := Position{ObservedSlot: 10, LP: 10, Value: 97_000_000, PoolLP: 100, PoolValue: 970_000_000}
	got := attributeEarnings(h, p, days, earningsTime(3), []*USDCRaw{cost(0), cost(0), cost(20_000_000), cost(20_000_000)})
	assertEarnings(t, got, -1, -2, -3)
	if got.Days[1].PerformanceUsd != 0 || got.Days[1].SellingCostChangeUsd != -2 || got.Days[2].PerformanceUsd != -1 {
		t.Fatalf("incorrect daily attribution: %+v", got.Days)
	}
}
func TestDepositAndTopupDoNotEarnTheExistingAllowance(t *testing.T) {
	first := Deposit{Flow: flowAt(1, 1, 10, 90, 100), Amount: 100_000_000}
	p := Position{ObservedSlot: 10, LP: 10, Value: 100_000_000, PoolLP: 100, PoolValue: 1_000_000_000}
	assertEarnings(t, attributeEarnings(History{Entries: []Entry{first}}, p, nil, earningsTime(1), []*USDCRaw{cost(20_000_000), cost(20_000_000)}), 0, 0, 0)
	topup := Deposit{Flow: flowAt(1, 2, 10, 100, 110), Amount: 100_000_000}
	p.LP, p.Value, p.PoolLP, p.PoolValue = 20, 200_000_000, 110, 1_100_000_000
	assertEarnings(t, attributeEarnings(History{Entries: []Entry{topup, first}}, p, nil, earningsTime(1), []*USDCRaw{cost(20_000_000), cost(20_000_000), cost(20_000_000)}), 0, 0, 0)
}
func TestRequestCancelAndClaimKeepCashAmounts(t *testing.T) {
	deposit := Deposit{Flow: flowAt(1, 1, 10, 90, 100), Amount: 100_000_000}
	escrow := &Escrow{LP: 4}
	request := Request{Flow: flowAt(2, 2, -4, 100, 100), Escrow: escrow, Requested: cost(40_000_000)}
	p := Position{ObservedSlot: 10, LP: 6, Value: 58_800_000, PoolLP: 100, PoolValue: 980_000_000, Withdrawal: &PendingWithdrawal{EscrowedLP: 4, Payout: 39_200_000}}
	assertEarnings(t, attributeEarnings(History{Entries: []Entry{request, deposit}}, p, nil, earningsTime(2), []*USDCRaw{cost(20_000_000), cost(0), cost(20_000_000)}), 0, -2, -2)
	cancel := Cancel{flowAt(3, 3, 4, 100, 100)}
	p.LP, p.Value, p.Withdrawal = 10, 98_000_000, nil
	assertEarnings(t, attributeEarnings(History{Entries: []Entry{cancel, request, deposit}}, p, nil, earningsTime(3), []*USDCRaw{cost(20_000_000), cost(20_000_000), cost(0), cost(20_000_000)}), 0, -2, -2)
	claim := Claim{Flow: flowAt(3, 3, 0, 100, 96), Escrow: escrow, Paid: cost(39_200_000)}
	p.LP, p.Value, p.PoolLP, p.PoolValue = 6, 58_800_000, 96, 940_800_000
	assertEarnings(t, attributeEarnings(History{Entries: []Entry{claim, request, deposit}}, p, nil, earningsTime(3), []*USDCRaw{cost(20_000_000), cost(20_000_000), cost(0), cost(20_000_000)}), 0, -2, -2)
	if *claim.Paid != 39_200_000 || p.Value != 58_800_000 {
		t.Fatal("display attribution changed cash values")
	}
}
func TestMissingCostEvidenceIsUnavailable(t *testing.T) {
	h := History{Entries: []Entry{Deposit{Flow: flowAt(1, 1, 10, 90, 100), Amount: 100_000_000}}}
	p := Position{ObservedSlot: 2, LP: 10, Value: 98_000_000, PoolLP: 100}
	for _, costs := range [][]*USDCRaw{{nil, cost(20_000_000)}, {cost(0), nil}} {
		if got := attributeEarnings(h, p, nil, earningsTime(1), costs); got != nil {
			t.Fatalf("invented performance without evidence: %+v", got)
		}
	}
}

func TestTopupDuringPendingWithdrawalDoesNotCreatePerformance(t *testing.T) {
	deposit := Deposit{Flow: flowAt(1, 1, 10, 90, 100), Amount: 100_000_000}
	escrow := &Escrow{LP: 4}
	request := Request{Flow: flowAt(1, 2, -4, 100, 100), Escrow: escrow, Requested: cost(40_000_000)}
	topup := Deposit{Flow: flowAt(1, 3, 10, 100, 110), Amount: 98_000_000}
	topup.PoolValueBefore, topup.PoolValueAfter = 980_000_000, 1_078_000_000
	p := Position{ObservedSlot: 10, LP: 16, Value: 156_800_000, PoolLP: 110, PoolValue: 1_078_000_000, Withdrawal: &PendingWithdrawal{EscrowedLP: 4, Payout: 39_200_000}}
	assertEarnings(t, attributeEarnings(History{Entries: []Entry{topup, request, deposit}}, p, nil, earningsTime(1), []*USDCRaw{cost(20_000_000), cost(0), cost(0), cost(20_000_000)}), 0, -2, -2)
}

func TestMissingDayDoesNotMoveTwoDaysReturnToToday(t *testing.T) {
	h := History{Entries: []Entry{Deposit{Flow: flowAt(1, 1, 10, 90, 100), Amount: 100_000_000}}}
	p := Position{ObservedSlot: 10, LP: 10, Value: 102_000_000, PoolLP: 100}
	got := attributeEarnings(h, p, []DayEnd{{At: earningsTime(1).Add(10 * time.Hour), Price: 10_000_000, PoolLP: 100}}, earningsTime(3), []*USDCRaw{cost(0), cost(0), cost(0)})
	assertEarnings(t, got, 2, 0, 2)
	if len(got.Days) != 1 || got.Days[0].Date != "2026-10-01" {
		t.Fatalf("invented daily values: %+v", got.Days)
	}
}

func TestFullClaimAndRequestCapReconcile(t *testing.T) {
	deposit := Deposit{Flow: flowAt(1, 1, 10, 90, 100), Amount: 100_000_000}
	escrow := &Escrow{LP: 10}
	request := Request{Flow: flowAt(1, 2, -10, 100, 100), Escrow: escrow, Requested: cost(100_000_000)}
	p := Position{ObservedSlot: 10, PoolLP: 100, PoolValue: 1_010_000_000, Withdrawal: &PendingWithdrawal{EscrowedLP: 10, Payout: 100_000_000}}
	assertEarnings(t, attributeEarnings(History{Entries: []Entry{request, deposit}}, p, nil, earningsTime(1), []*USDCRaw{cost(0), cost(0), cost(20_000_000)}), 0, 0, 0)
	claim := Claim{Flow: flowAt(1, 3, 0, 100, 90), Escrow: escrow, Paid: cost(98_000_000)}
	p.Withdrawal = nil
	p.PoolLP = 90
	assertEarnings(t, attributeEarnings(History{Entries: []Entry{claim, request, deposit}}, p, nil, earningsTime(1), []*USDCRaw{cost(20_000_000), cost(0), cost(0), cost(20_000_000)}), 0, -2, -2)
}

func TestEarningsUsesThirtyUTCCalendarDays(t *testing.T) {
	now := earningsTime(26)
	var days []DayEnd
	costs := []*USDCRaw{}
	for i := 35; i > 0; i-- {
		days = append(days, DayEnd{At: now.AddDate(0, 0, -i).Add(11 * time.Hour), Price: 1, PoolLP: 1})
		costs = append(costs, cost(0))
	}
	costs = append(costs, cost(0))
	got := attributeEarnings(History{}, Position{}, days, now, costs)
	if got == nil || len(got.Days) != 30 || got.Days[0].Date != "2026-09-27" || got.Days[29].Date != "2026-10-26" {
		t.Fatalf("wrong thirty-day window: %+v", got)
	}
}
