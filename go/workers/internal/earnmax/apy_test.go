package earnmax

import (
	"testing"
	"time"
)

func TestShareAPYLastPointIsTrailingWeek(t *testing.T) {
	var days []DailyStat
	for i := range 8 {
		days = append(days, DailyStat{Day: time.Date(2026, 10, 10+i, 0, 0, 0, 0, time.UTC).Format(time.DateOnly), TVLRaw: 1_000_000 + float64(i)*100, LPRaw: 1_000_000})
	}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	got := ShareAPY(days, &LiveShare{LP: 1_000_000, TVL: 997_600}, now)
	if got.Bps == nil {
		t.Fatal("headline undefined")
	}
	last := got.History[len(got.History)-1]
	if last.APYBps != *got.Bps {
		t.Fatalf("last point %d, headline %d", last.APYBps, *got.Bps)
	}
}

func TestShareAPYHistoryStartsWhereAFullWindowExists(t *testing.T) {
	var days []DailyStat
	for i := range 10 {
		days = append(days, DailyStat{Day: time.Date(2026, 10, 10+i, 0, 0, 0, 0, time.UTC).Format(time.DateOnly), TVLRaw: 1_000_000 + float64(i)*100, LPRaw: 1_000_000})
	}
	got := ShareAPY(days, nil, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	if first := got.History[0].ObservedAt; first != "2026-10-17T23:59:59Z" {
		t.Fatalf("first history point %s, want 2026-10-17T23:59:59Z", first)
	}
}
